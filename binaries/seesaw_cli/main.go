// Copyright 2012 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Author: jsing@google.com (Joel Sing)

// The seesaw_cli binary implements the Seesaw v2 Command Line Interface (CLI),
// which allows for user control of the Seesaw v2 Engine component.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"
	"time"

	"github.com/wy2745/seesaw/cli"
	"github.com/wy2745/seesaw/common/conn"
	"github.com/wy2745/seesaw/common/ipc"
	"github.com/wy2745/seesaw/common/seesaw"

	"golang.org/x/crypto/ssh/terminal"
)

var (
	command      = flag.String("c", "", "Command to execute")
	engineSocket = flag.String("engine", seesaw.EngineSocket, "Seesaw Engine Socket")

	oldTermState *terminal.State
	prompt       string
	// prompt is a string that is written at the start of each input line (i.e.
	// "> ").
	seesawCLI    *cli.SeesawCLI
	seesawConn   *conn.Seesaw
	term         *terminal.Terminal
)

func exit() {
	if oldTermState != nil {
		terminal.Restore(syscall.Stdin, oldTermState) //将输出重新定位回原来的file去
	}
	fmt.Printf("\n")
	os.Exit(0)
}

func fatalf(format string, a ...interface{}) {
	if oldTermState != nil {
		terminal.Restore(syscall.Stdin, oldTermState)
	}
	fmt.Fprintf(os.Stderr, format, a...)
	fmt.Fprintf(os.Stderr, "\n")
	os.Exit(1)
}

func suspend() {
	if oldTermState != nil {
		terminal.Restore(syscall.Stdin, oldTermState)
	}
	go resume()
	syscall.Kill(os.Getpid(), syscall.SIGTSTP)
}

func resume() {
	time.Sleep(1 * time.Second)
	fmt.Println("resuming...")
	terminalInit()
}

//初始化terminal
func terminalInit() {
	var err error
	oldTermState, err = terminal.MakeRaw(syscall.Stdin) //记录旧terminal
	if err != nil {
		fatalf("Failed to get raw terminal: %v", err)
	}

	term = terminal.NewTerminal(os.Stdin, prompt)  //新建一个terminal，输出以prompt开头
	//设置一些按键
	term.AutoCompleteCallback = autoComplete
}

// commandChain builds a command chain from the given command slice.
func commandChain(chain []*cli.Command, args []string) string {
	s := make([]string, 0)
	for _, c := range chain {
		s = append(s, c.Command)
	}
	s = append(s, args...)
	if len(s) > 0 && len(args) == 0 {
		s = append(s, "")
	}
	return strings.Join(s, " ")
}

// autoComplete attempts to complete the user's input when certain
// characters are typed.
func autoComplete(line string, pos int, key rune) (string, int, bool) {
	switch key {
	case 0x01: // Ctrl-A
		return line, 0, true
	case 0x03: // Ctrl-C
		exit()
	case 0x05: // Ctrl-E
		return line, len(line), true
	case 0x09: // Ctrl-I (Tab)
		_, _, chain, args := cli.FindCommand(string(line))
		line := commandChain(chain, args)
		return line, len(line), true
	case 0x15: // Ctrl-U
		return "", 0, true
	case 0x1a: // Ctrl-Z
		suspend()
	case '?':
		cmd, subcmds, chain, args := cli.FindCommand(string(line[0:pos]))
		if cmd == nil {
			term.Write([]byte(prompt))
			term.Write([]byte(line))
			term.Write([]byte("?\n"))
		}
		if subcmds != nil {
			for _, c := range *subcmds {
				term.Write([]byte(" " + c.Command))
				term.Write([]byte("\n"))
			}
		} else if cmd == nil {
			term.Write([]byte("Unknown command.\n"))
		}

		line := commandChain(chain, args)
		return line, len(line), true
	}
	return "", 0, false
}

// interactive invokes the interactive CLI interface.
func interactive() {
	status, err := seesawConn.ClusterStatus()
	if err != nil {
		fatalf("Failed to get cluster status: %v", err)
	}
	fmt.Printf("\nSeesaw CLI - Engine version %d\n\n", status.Version)

	u, err := user.Current()
	if err != nil {
		fatalf("Failed to get current user: %v", err)
	}

	ha, err := seesawConn.HAStatus()
	if err != nil {
		fatalf("Failed to get HA status: %v", err)
	}
	if ha.State != seesaw.HAMaster {
		fmt.Println("WARNING: This seesaw is not currently the master.")
	}

	prompt = fmt.Sprintf("%s@%s> ", u.Username, status.Site)

	// Setup signal handler before we switch to a raw terminal.
	sigc := make(chan os.Signal, 3)
	//只要收到以下三个signal，就退出
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	go func() {
		<-sigc
		exit()
	}()


	terminalInit()

	for {
		cmdline, err := term.ReadLine()
		if err != nil {
			break
		}
		//获取cmd
		cmdline = strings.TrimSpace(cmdline)
		if cmdline == "" {
			continue
		}
		//执行cmd
		if err := seesawCLI.Execute(cmdline); err != nil {
			fmt.Println(err)
		}
	}
}

func main() {
	flag.Parse()

	//为组件创建一个新的context
	ctx := ipc.NewTrustedContext(seesaw.SCLocalCLI)

	var err error
	//建立一个新的ipc连接
	seesawConn, err = conn.NewSeesawIPC(ctx)

	if err != nil {
		fatalf("Failed to connect to engine: %v", err)
	}
	if err := seesawConn.Dial(*engineSocket); err != nil {
		fatalf("Failed to connect to engine: %v", err)
	}
	defer seesawConn.Close()
	//将engine和cli进行连接
	seesawCLI = cli.NewSeesawCLI(seesawConn, exit)

	//如果没有指令，那么循环等待
	if *command == "" {
		interactive()
		exit()
	}
	//如果有指令，执行
	if err := seesawCLI.Execute(*command); err != nil {
		fatalf("%v", err)
	}
}
