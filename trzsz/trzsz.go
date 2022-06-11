/*
MIT License

Copyright (c) 2022 Lonny Wong

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package trzsz

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ncruces/zenity"
	"golang.org/x/term"
)

var gDragFiles []string = nil
var gDragHasDir bool = false
var gInterrupting bool = false
var gTransfer *TrzszTransfer = nil
var trzszRegexp = regexp.MustCompile("::TRZSZ:TRANSFER:([SR]):(\\d+\\.\\d+\\.\\d+)(:\\d+)?")

func printVersion() {
	fmt.Printf("trzsz go %s\n", kTrzszVersion)
}

func printHelp() {
	fmt.Print("usage: trzsz [-h] [-v] [-d] command line\n\n" +
		"Wrapping command line to support trzsz ( trz / tsz ).\n\n" +
		"positional arguments:\n" +
		"  command line       the original command line\n\n" +
		"optional arguments:\n" +
		"  -h, --help         show this help message and exit\n" +
		"  -v, --version      show version number and exit\n" +
		"  -d, --dragfile     enable drag file(s) to upload\n")
}

func getTrzszConfig(name string) *string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	file, err := os.Open(filepath.Join(home, ".trzsz.conf"))
	if err != nil {
		return nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		if strings.TrimSpace(line[0:idx]) == name {
			value := strings.TrimSpace(line[idx+1:])
			if len(value) == 0 {
				return nil
			}
			return &value
		}
	}
	return nil
}

func detectTrzsz(output []byte) (*byte, bool) {
	if !bytes.Contains(output, []byte("::TRZSZ:TRANSFER:")) {
		return nil, false
	}
	match := trzszRegexp.FindSubmatch(output)
	if len(match) < 2 {
		return nil, false
	}
	remoteIsWindows := len(match) > 3 && string(match[3]) == ":1"
	return &match[1][0], remoteIsWindows
}

func chooseDownloadPath() (string, error) {
	savePath := getTrzszConfig("DefaultDownloadPath")
	if savePath != nil {
		return *savePath, nil
	}
	path, err := zenity.SelectFile(
		zenity.Title("Choose a folder to save file(s)"),
		zenity.Directory(),
		zenity.ShowHidden(),
	)
	if err != nil {
		return "", err
	}
	if len(path) == 0 {
		return "", zenity.ErrCanceled
	}
	return path, nil
}

func chooseUploadFiles() ([]string, error) {
	if gDragFiles != nil {
		files := gDragFiles
		gDragFiles = nil
		gDragHasDir = false
		return files, nil
	}
	options := []zenity.Option{
		zenity.Title("Choose some files to send"),
		zenity.ShowHidden(),
	}
	defaultPath := getTrzszConfig("DefaultUploadPath")
	if defaultPath != nil {
		options = append(options, zenity.Filename(*defaultPath))
	}
	files, err := zenity.SelectFileMutiple(options...)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, zenity.ErrCanceled
	}
	return files, nil
}

func newProgressBar(pty *TrzszPty, config map[string]interface{}) (*TextProgressBar, error) {
	quiet := false
	if v, ok := config["quiet"].(bool); ok {
		quiet = v
	}
	if quiet {
		return nil, nil
	}
	columns, err := pty.GetColumns()
	if err != nil {
		return nil, err
	}
	tmuxPaneColumns := -1
	if v, ok := config["tmux_pane_width"].(float64); ok {
		tmuxPaneColumns = int(v)
	}
	return NewTextProgressBar(os.Stdout, columns, tmuxPaneColumns), nil
}

func downloadFiles(pty *TrzszPty, transfer *TrzszTransfer, remoteIsWindows bool) error {
	path, err := chooseDownloadPath()
	if err == zenity.ErrCanceled {
		return transfer.sendAction(false, remoteIsWindows)
	}
	if err != nil {
		return err
	}
	if err := checkPathWritable(path); err != nil {
		return err
	}

	if err := transfer.sendAction(true, remoteIsWindows); err != nil {
		return err
	}
	config, err := transfer.recvConfig()
	if err != nil {
		return err
	}

	progress, err := newProgressBar(pty, config)
	if err != nil {
		return err
	}
	if progress != nil && !reflect.ValueOf(progress).IsNil() {
		pty.OnResize(func(cols int) { progress.setTerminalColumns(cols) })
		defer pty.OnResize(nil)
	}

	localNames, err := transfer.recvFiles(path, progress)
	if err != nil {
		return err
	}

	return transfer.clientExit(fmt.Sprintf("Saved %s to %s", strings.Join(localNames, ", "), path))
}

func uploadFiles(pty *TrzszPty, transfer *TrzszTransfer, remoteIsWindows bool) error {
	files, err := chooseUploadFiles()
	if err == zenity.ErrCanceled {
		return transfer.sendAction(false, remoteIsWindows)
	}
	if err != nil {
		return err
	}
	if err := checkFilesReadable(files); err != nil {
		return err
	}

	if err := transfer.sendAction(true, remoteIsWindows); err != nil {
		return err
	}
	config, err := transfer.recvConfig()
	if err != nil {
		return err
	}

	progress, err := newProgressBar(pty, config)
	if err != nil {
		return err
	}
	if progress != nil && !reflect.ValueOf(progress).IsNil() {
		pty.OnResize(func(cols int) { progress.setTerminalColumns(cols) })
		defer pty.OnResize(nil)
	}

	remoteNames, err := transfer.sendFiles(files, progress)
	if err != nil {
		return err
	}

	return transfer.clientExit(fmt.Sprintf("Received %s", strings.Join(remoteNames, ", ")))
}

func handleTrzsz(pty *TrzszPty, mode byte, remoteIsWindows bool) {
	transfer := NewTransfer(pty.Stdin, nil)

	gTransfer = transfer
	defer func() {
		gTransfer = nil
	}()

	defer func() {
		if err := recover(); err != nil {
			transfer.clientError(NewTrzszError(fmt.Sprintf("%v", err), "panic", true))
		}
	}()

	var err error
	if mode == 'S' {
		err = downloadFiles(pty, transfer, remoteIsWindows)
	} else if mode == 'R' {
		err = uploadFiles(pty, transfer, remoteIsWindows)
	}
	if err != nil {
		transfer.clientError(err)
	}
}

func uploadDragFiles(pty *TrzszPty) {
	time.Sleep(300 * time.Millisecond)
	if gDragFiles == nil {
		return
	}
	gInterrupting = true
	pty.Stdin.Write([]byte{0x03})
	time.Sleep(200 * time.Millisecond)
	gInterrupting = false
	if gDragHasDir {
		pty.Stdin.Write([]byte("echo 'upload directory is not supported yet'\n"))
	} else {
		pty.Stdin.Write([]byte("trz\n"))
	}
	time.Sleep(time.Second)
	if gDragFiles != nil {
		gDragFiles = nil
		gDragHasDir = false
	}
}

func wrapInput(pty *TrzszPty, dragFile bool) {
	buffer := make([]byte, 10240)
	for {
		n, err := os.Stdin.Read(buffer)
		if err == io.EOF {
			if IsWindows() { // ctrl + z
				n = 1
				err = nil
				buffer[0] = 0x1A
			} else {
				pty.Stdin.Close()
				break
			}
		}
		if err == nil && n > 0 {
			buf := buffer[0:n]
			if transfer := gTransfer; transfer != nil {
				if buf[0] == '\x03' { // `ctrl + c` to stop transferring files
					transfer.stopTransferringFiles()
				}
				continue
			}
			if dragFile {
				dragFiles, hasDir := detectDragFiles(buf)
				if dragFiles != nil {
					if gDragFiles == nil {
						gDragFiles = dragFiles
						gDragHasDir = hasDir
						go uploadDragFiles(pty)
					} else {
						gDragFiles = append(gDragFiles, dragFiles...)
						gDragHasDir = gDragHasDir || hasDir
					}
					continue
				} else if gDragFiles != nil {
					gDragFiles = nil
					gDragHasDir = false
				}
			}
			pty.Stdin.Write(buf)
		}
	}
}

func wrapOutput(pty *TrzszPty, dragFile bool) {
	const bufSize = 10240
	buffer := make([]byte, bufSize)
	for {
		n, err := pty.Stdout.Read(buffer)
		if err == io.EOF {
			os.Stdout.Close()
			break
		} else if err == nil && n > 0 {
			buf := buffer[0:n]
			if transfer := gTransfer; transfer != nil {
				transfer.addReceivedData(buf)
				buffer = make([]byte, bufSize)
				continue
			}
			mode, remoteIsWindows := detectTrzsz(buf)
			if mode != nil {
				os.Stdout.Write(bytes.Replace(buf, []byte("TRZSZ"), []byte("TRZSZGO"), 1))
				go handleTrzsz(pty, *mode, remoteIsWindows)
				continue
			}
			if gInterrupting {
				continue
			}
			if dragFile && gDragFiles != nil {
				output := strings.TrimRight(string(trimVT100(buf)), "\r\n")
				if output == "trz" {
					os.Stdout.WriteString("\r\n")
					continue
				}
				if output == "" || output == "\"" {
					continue
				}
			}
			os.Stdout.Write(buf)
		}
	}
}

func handleSignal(pty *TrzszPty) {
	sigterm := make(chan os.Signal, 1)
	signal.Notify(sigterm, syscall.SIGTERM)
	go func() {
		<-sigterm
		pty.Terminate()
	}()

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	go func() {
		for {
			<-sigint
			if transfer := gTransfer; transfer != nil {
				transfer.stopTransferringFiles()
			}
		}
	}()
}

// TrzszMain entry of trzsz client
func TrzszMain() int {
	// parse command line arguments
	if len(os.Args) < 2 {
		printHelp()
		return 0
	}
	if os.Args[1] == "-h" || os.Args[1] == "--help" {
		printHelp()
		return 0
	}
	if os.Args[1] == "-v" || os.Args[1] == "--version" {
		printVersion()
		return 0
	}

	// drag file option
	name := os.Args[1]
	args := os.Args[2:]
	dragFile := false
	if os.Args[1] == "-d" || os.Args[1] == "--dragfile" {
		if len(os.Args) < 3 {
			printHelp()
			return 0
		}
		dragFile = true
		name = os.Args[2]
		args = os.Args[3:]
	}

	// spawn a pty
	pty, err := Spawn(name, args...)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return -1
	}
	defer func() { pty.Close() }()

	// set stdin in raw mode
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return -2
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), state) }()

	// wrap input and output
	go wrapInput(pty, dragFile)
	go wrapOutput(pty, dragFile)

	// handle signal
	go handleSignal(pty)

	// wait for exit
	pty.Wait()
	return pty.ExitCode()
}
