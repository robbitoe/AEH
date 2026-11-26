package egress

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// LauncherFailed is the exit code of a launcher that could not start its forwarder,
// reach the proxy or start the agent's program, or whose forwarder stopped. aeh treats
// it as an infrastructure failure of an agent launch with egress.
const LauncherFailed = 251

const (
	// forwardMax bounds the forwarder's open connections; the proxy's own limit is lower,
	// so extra connections only ever get the proxy's refusal.
	forwardMax = 256
	// forwardIdle closes a relay with no traffic either way, beyond the proxy's idle time.
	forwardIdle = 20 * time.Minute
)

// Forward accepts connections on l and relays each to the proxy's Unix socket. The proxy
// applies the policy; the forwarder bounds its own connections and idle relays.
func Forward(l net.Listener, socket string) error {
	return forward(l, socket, forwardIdle)
}

func forward(l net.Listener, socket string, idle time.Duration) error {
	slots := make(chan struct{}, forwardMax)
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			u, err := net.DialTimeout("unix", socket, 10*time.Second)
			if err != nil {
				_ = c.Close()
				return
			}
			splice(c, u, nil, idle, nil)
		}()
	}
}

// waitSocket waits until the proxy accepts connections on its socket.
func waitSocket(socket string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		c, err := net.DialTimeout("unix", socket, time.Second)
		if err == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the egress proxy did not answer on %s within %s: %w", socket, wait, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Launch starts the forwarder on listen, waits for the proxy on socket, then runs argv
// with this process's standard streams and environment, and returns the exit code to
// use. Signals are passed to the program. If the forwarder stops, the program is killed
// and the launch ends with LauncherFailed.
func Launch(socket, listen string, wait time.Duration, argv []string, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "aeh-egress: "+format+"\n", a...)
		return LauncherFailed
	}
	if len(argv) == 0 {
		return fail("no program to run")
	}
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return fail("start the forwarder: %v", err)
	}
	defer l.Close()
	if err := waitSocket(socket, wait); err != nil {
		return fail("%v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- Forward(l, socket) }()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fail("start %s: %v", argv[0], err)
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sigs)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case s := <-sigs:
			_ = cmd.Process.Signal(s)
		case err := <-done:
			return exitCode(cmd, err)
		case err := <-stopped:
			_ = cmd.Process.Kill()
			<-done
			return fail("the forwarder stopped: %v", err)
		}
	}
}

func exitCode(cmd *exec.Cmd, err error) int {
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return LauncherFailed
	}
	ps := cmd.ProcessState
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}
