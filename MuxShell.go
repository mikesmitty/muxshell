package muxshell

// Originally based off http://stackoverflow.com/questions/24440193
// Original code licensed CC-BY-SA 3.0: https://creativecommons.org/licenses/by-sa/3.0/

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/howbazaar/loggo"

	"golang.org/x/crypto/ssh"
)

type Shell struct {
	stdin   chan<- string
	stdout  <-chan []byte
	client  *ssh.Client
	session *ssh.Session
	User    string
	Server  string
	Port    int
	PsOne   string
	LastRun time.Time
	closed  bool
}

// 1MB should be enough for most small jobs. If it runs out of buffer space
// you'll need to increase this
const bufferSize = 1024 * 1024

var logger = loggo.GetLogger("hex.muxshell")
var conns = make(map[string]*Shell)
var mutex = &sync.Mutex{}

// Set our proactive connectino timeout period. If we haven't run a command in 10 minutes start a new conn
var timeout, durErr = time.ParseDuration("10m")

func Connect(sshUser, serverName string, sshPort int) (*Shell, error) {
	var err error

	switch {
	case sshUser == "":
		return nil, fmt.Errorf("Invalid ssh user")
	case serverName == "":
		return nil, fmt.Errorf("Invalid ssh server")
	case sshPort < 1 || sshPort > 65535:
		return nil, fmt.Errorf("Invalid ssh port")
	}

	ident := fmt.Sprintf("%s@%s:%d", sshUser, serverName, sshPort)
	logger.Debugf("Connecting to %s", ident)
	conn, exists := conns[ident]
	switch {
	case exists && !conn.closed:
		return conn, nil
	case exists && conn.closed, !exists:
		mutex.Lock()
		conns[ident], err = makeConnection(sshUser, serverName, sshPort)
		mutex.Unlock()
		if err != nil {
			return nil, err
		}
	}

	return conns[ident], nil
}

func makeConnection(sshUser, serverName string, sshPort int) (*Shell, error) {
	shell := &Shell{}

	keys, err := GetSSHKeys()

	config := &ssh.ClientConfig{
		User: sshUser,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(keys...),
		},
	}

	hostPort := fmt.Sprintf("%s:%d", serverName, sshPort)
	client, err := ssh.Dial("tcp", hostPort, config)
	if err != nil {
		return shell, err
	}

	session, err := client.NewSession()

	if err != nil {
		return shell, err
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,     // disable echoing
		ssh.TTY_OP_ISPEED: 14400, // input speed = 14.4kbaud
		ssh.TTY_OP_OSPEED: 14400, // output speed = 14.4kbaud
	}

	if err := session.RequestPty("xterm", 40, 200, modes); err != nil {
		return shell, err
	}

	w, err := session.StdinPipe()
	if err != nil {
		return shell, err
	}
	r, err := session.StdoutPipe()
	if err != nil {
		return shell, err
	}
	in, out := startShell(sshUser, serverName, sshPort, w, r)
	if err := session.Start("/bin/sh"); err != nil {
		return shell, err
	}
	initOutput := <-out //Store the PS1 so we can filter it out later
	psOne := string(initOutput)

	// Disable command echoing and mail checking so we don't have to filter it out of our output
	in <- "stty -echo; unset MAILCHECK; unset MAIL"
	<-out

	shell.stdin = in
	shell.stdout = out
	shell.client = client
	shell.session = session
	shell.PsOne = strings.TrimLeft(psOne, "\n\r \t")
	shell.User = sshUser
	shell.Server = serverName
	shell.Port = sshPort
	shell.LastRun = time.Now()

	return shell, err
}

func getPsOne(sshUser, serverName string, sshPort int) {
}

func (sh *Shell) Run(command string) (string, error) {
	var err error

	if sh.closed {
		return "", fmt.Errorf("SSH connection closed")
	}
	elapsed := time.Since(sh.LastRun)
	if elapsed > timeout {
		logger.Debugf("%s has elapsed since last command. Reconnecting", elapsed)
		err = sh.Reconnect()
		if err != nil {
			return "", err
		}
	}
	sh.LastRun = time.Now()
	sh.stdin <- command
	stdout := <-sh.stdout
	end := len(stdout) - len(sh.PsOne)
	if stdout[len(stdout)-1] == '\n' || stdout[len(stdout)-1] == '\r' {
		end--
	}
	output := string(stdout[:end])

	return output, err
}

func (sh *Shell) Reconnect() error {
	logger.Warningf("Reconnecting to %s@%s:%d", sh.User, sh.Server, sh.Port)
	sshUser := sh.User
	sshServer := sh.Server
	sshPort := sh.Port
	newsh, err := Connect(sshUser, sshServer, sshPort)
	if err != nil {
		return err
	}

	sh.stdin = newsh.stdin
	sh.stdout = newsh.stdout
	sh.client = newsh.client
	sh.session = newsh.session
	sh.closed = false
	sh.LastRun = time.Now()

	return err
}

func (sh *Shell) Exit() {
	sh.stdin <- "exit"
	sh.session.Wait()
	sh.session.Close()
	sh.client.Close()
}

func ExitAll() {
	for _, conn := range conns {
		conn.Exit()
	}
}

func InvalidateConn(sshUser, serverName string, sshPort int) {
	ident := fmt.Sprintf("%s@%s:%s", sshUser, serverName, sshPort)
	conn, exists := conns[ident]
	if exists {
		mutex.Lock()
		conn.closed = true
		mutex.Unlock()
	}
}

func startShell(sshUser, serverName string, sshPort int, w io.Writer, r io.Reader) (chan<- string, <-chan []byte) {
	psOneStart := []byte("sh-")
	var psOneEnd []byte
	if sshUser == "root" {
		psOneEnd = []byte("# ") //assuming the $PS1 == 'sh-4.3# '
	} else {
		psOneEnd = []byte("$ ") //assuming the $PS1 == 'sh-4.3$ '
	}

	in := make(chan string, 1)
	out := make(chan []byte, 1)
	var wg sync.WaitGroup
	wg.Add(1) //for the shell itself
	go func() {
		for cmd := range in {
			wg.Add(1)
			logger.Debugf("Command[%s]: |%s|", serverName, cmd)
			w.Write([]byte(strings.TrimRightFunc(cmd, unicode.IsSpace) + "\n"))
			wg.Wait()
		}
	}()
	go func() {
		var (
			buf [bufferSize]byte
			t   int
		)
		for {
			n, err := r.Read(buf[t:])
			if err != nil {
				close(in)
				close(out)
				InvalidateConn(sshUser, serverName, sshPort)
				return
			}
			t += n
			if t >= 8 { // NOBODY PANIC! due to invalid buffer indexes
				// Check against the start and end of the $PS1
				if bytes.Equal(buf[t-2:t], psOneEnd) && bytes.Equal(buf[t-8:t-5], psOneStart) {
					logger.Debugf("Output[%s]: |%s|", serverName, string(buf[:t]))
					out <- buf[:t]
					t = 0
					wg.Done()
				}
			}
		}
	}()

	return in, out
}

func GetSSHKeys(params ...string) ([]ssh.Signer, error) {
	// Use the default path if our path is empty, and create a relative path
	// from cwd if path is not absolute
	dirPath := ""
	if len(params) > 0 {
		dirPath = params[0]
	}
	if dirPath == "" {
		dirPath = fmt.Sprintf("%s/.ssh/", os.Getenv("HOME"))
	}
	//else if dirPath[0] != "/" {
	//	cwd, err := os.Getwd()
	//	if err != nil {
	//		logger.Errorf("%v", err)
	//		return nil, err
	//	}
	//	dirPath = fmt.Sprintf("%s/%s", cwd, dirPath)
	//}

	// Grab listing of files in supplied path
	files, err := ioutil.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}

	// Grab the private keys only from the directory, based on name and
	// get the private key file contents
	privateKeys := make([]ssh.Signer, 0)
	for _, file := range files {
		rPriv, err := regexp.Compile("id_.*$")
		rPub, err := regexp.Compile("id_.*\\.pub$")
		if err != nil {
			return nil, err
		}

		// Read the private key file contents and parse the key
		if rPriv.MatchString(file.Name()) && !rPub.MatchString(file.Name()) {
			keyFile := fmt.Sprintf("%s/%s", dirPath, file.Name())
			privKey, err := ioutil.ReadFile(keyFile)
			if err != nil {
				return nil, err
			}

			key, err := ssh.ParsePrivateKey(privKey)
			if err != nil {
				return nil, err
			}

			privateKeys = append(privateKeys, key)
		}
	}

	return privateKeys, err
}
