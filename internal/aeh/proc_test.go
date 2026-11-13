package aeh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Each helper holds an inherited log handle and a TCP connection until it exits.
// EOF on all three connections proves that even the grandchild has stopped,
// without depending on platform-specific PID probes (or Unix zombie reaping).
func TestProcessTreeCancellation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, runner := range []string{"agent", "shell"} {
		for _, cause := range []string{"cancel", "deadline"} {
			t.Run(runner+"/"+cause, func(t *testing.T) {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				parentTimeout := 5 * time.Second
				if runner == "shell" && cause == "deadline" {
					parentTimeout = 15 * time.Second // exercise the gate's own timeout
				}
				ctx, cancel := context.WithTimeout(context.Background(), parentTimeout)
				defer cancel()
				args := []string{exe, "-test.run=^TestProcessTreeHelper$", "--", "aeh-proc-helper", "2", listener.Addr().String()}
				dir := t.TempDir()
				done := make(chan error, 1)
				go func() {
					var err error
					if runner == "agent" {
						_, err = runAgent(ctx, Profile{Command: args}, nil, dir, "", dir, "agent")
					} else {
						shellDir := dir
						if runtime.GOOS == "windows" {
							// Launch from the executable's directory so cmd needs no
							// quoting of a temporary path that may contain spaces.
							shellDir = filepath.Dir(exe)
							args[0] = filepath.Base(exe)
						}
						quoted := make([]string, len(args))
						for i, arg := range args {
							if runtime.GOOS == "windows" {
								quoted[i] = arg
							} else {
								quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
							}
						}
						command := strings.Join(quoted, " ")
						_, err = runShell(ctx, shellDir, command, 5*time.Second, nil)
					}
					done <- err
				}()
				var connections []net.Conn
				for i := 0; i < 3; i++ {
					conn, err := listener.Accept()
					if err != nil {
						t.Fatalf("wait for helper %d: %v", i, err)
					}
					t.Cleanup(func() { conn.Close() })
					if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
						t.Fatal(err)
					}
					var pid int
					if _, err := fmt.Fscanln(bufio.NewReader(conn), &pid); err != nil {
						t.Fatalf("read helper PID: %v", err)
					}
					// Also clean up explicitly if the tree-kill assertion fails.
					process, err := os.FindProcess(pid)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						_ = process.Kill()
						_ = process.Release()
					})
					connections = append(connections, conn)
				}
				wantErr := context.DeadlineExceeded
				if cause == "cancel" {
					wantErr = context.Canceled
					cancel()
				}
				select {
				case err := <-done:
					if !errors.Is(err, wantErr) {
						t.Fatalf("run error = %v, want %v", err, wantErr)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("command did not return after cancellation")
				}
				deadline := time.Now().Add(2 * time.Second)
				for i, conn := range connections {
					if err := conn.SetReadDeadline(deadline); err != nil {
						t.Fatal(err)
					}
					var b [1]byte
					_, err := conn.Read(b[:])
					reset := syscall.Errno(10054) // WSAECONNRESET after TerminateProcess
					if !errors.Is(err, io.EOF) && !(runtime.GOOS == "windows" && errors.Is(err, reset)) {
						t.Errorf("helper %d still alive after cancellation: %v", i, err)
					}
				}
			})
		}
	}
}

func TestProcessTreeHelper(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "aeh-proc-helper" {
		return
	}
	depth, err := strconv.Atoi(os.Args[len(os.Args)-2])
	if err != nil {
		t.Fatal(err)
	}
	address := os.Args[len(os.Args)-1]
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if depth > 0 {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(exe, "-test.run=^TestProcessTreeHelper$", "--", "aeh-proc-helper", strconv.Itoa(depth-1), address)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		defer child.Wait()
	}
	time.Sleep(30 * time.Second)
	runtime.KeepAlive(conn)
}
