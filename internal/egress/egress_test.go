package egress

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckPair(t *testing.T) {
	for _, ok := range []string{"api.openai.com:443", "chatgpt.com:443", "a-b.example.org:8443", "8.8.8.8:53", "[2606:4700::1111]:443"} {
		if err := CheckPair(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"api.openai.com", "API.openai.com:443", "*.openai.com:443", "x.com:0", "x.com:65536", "x.com:0443",
		"-x.com:443", "x..com:443", "x.com.:443", "1.2.3:443", "2606:4700::1111:443", "x_y.com:443", ":443", "x.com:", "[example.com]:443"} {
		if err := CheckPair(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
	if err := CheckPairs([]string{"a.com:443", "a.com:443"}); err == nil {
		t.Error("duplicate accepted")
	}
	many := make([]string, MaxPairs+1)
	for i := range many {
		many[i] = fmt.Sprintf("h%d.com:443", i)
	}
	if err := CheckPairs(many); err == nil {
		t.Error("too many pairs accepted")
	}
}

func TestParsePolicy(t *testing.T) {
	if _, err := ParsePolicy([]byte(`{"allow":["a.com:443"],"max_connections":1,"max_bytes":1,"idle_seconds":1,"connect_seconds":1}`)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"allow":["a.com:443"],"max_connections":1,"max_bytes":1,"idle_seconds":1,"connect_seconds":1,"x":1}`,
		`{"allow":["a.com"],"max_connections":1,"max_bytes":1,"idle_seconds":1,"connect_seconds":1}`,
		`{"allow":[],"max_connections":0,"max_bytes":1,"idle_seconds":1,"connect_seconds":1}`} {
		if _, err := ParsePolicy([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestPublic(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111", "::ffff:8.8.8.8"} {
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("%s should be public", s)
		}
	}
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"255.255.255.255", "224.0.0.1", "198.18.0.1", "192.0.2.1", "::1", "::", "fe80::1", "fc00::1", "fd00:ec2::254",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "64:ff9b::a9fe:a9fe", "2002:a9fe:a9fe::1", "2001::1", "ff02::1", "2001:db8::1"} {
		if Public(netip.MustParseAddr(s)) {
			t.Errorf("%s should not be public", s)
		}
	}
}

// startProxy serves a proxy on a Unix socket. Names resolve to the local echo server and
// local addresses count as allowed, so the tunnel can be tested without a network.
func startProxy(t *testing.T, pol Policy, target string) (socket string, logs *syncBuffer) {
	t.Helper()
	dir, err := os.MkdirTemp("", "egr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket = filepath.Join(dir, "p.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	tap := netip.MustParseAddrPort(target)
	logs = &syncBuffer{}
	p := &Proxy{Policy: pol, Log: logs,
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "private.example.com" {
				return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
			}
			return []netip.Addr{tap.Addr()}, nil
		},
		Allowed: func(a netip.Addr) bool { return a == tap.Addr() }}
	go p.Serve(l)
	return socket, logs
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// echoServer answers each line with the same line upper-cased.
func echoServer(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				s := bufio.NewScanner(c)
				for s.Scan() {
					fmt.Fprintln(c, strings.ToUpper(s.Text()))
				}
			}()
		}
	}()
	return l.Addr().String()
}

func connect(t *testing.T, socket, request string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, request); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	return c, br, resp.StatusCode
}

func TestProxyTunnelsOnlyDeclaredPairs(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	pol := DefaultPolicy([]string{"api.example.com:" + port, "private.example.com:" + port})
	socket, logs := startProxy(t, pol, target)

	// A declared pair: the tunnel works, including bytes sent right after the request.
	c, br, code := connect(t, socket, "CONNECT api.example.com:"+port+" HTTP/1.1\r\nHost: api.example.com:"+port+"\r\n\r\nhello\n")
	if code != 200 {
		t.Fatalf("declared pair: %d", code)
	}
	if line, err := br.ReadString('\n'); err != nil || line != "HELLO\n" {
		t.Fatalf("tunnel: %q %v", line, err)
	}
	fmt.Fprintln(c, "again")
	if line, _ := br.ReadString('\n'); line != "AGAIN\n" {
		t.Fatalf("tunnel: %q", line)
	}
	c.Close()

	for name, tc := range map[string]struct {
		req  string
		code int
	}{
		"unlisted host":    {"CONNECT other.example.com:" + port + " HTTP/1.1\r\n\r\n", 403},
		"unlisted port":    {"CONNECT api.example.com:1 HTTP/1.1\r\n\r\n", 403},
		"address literal":  {"CONNECT " + target + " HTTP/1.1\r\n\r\n", 403},
		"private address":  {"CONNECT private.example.com:" + port + " HTTP/1.1\r\n\r\n", 403},
		"plain HTTP":       {"GET http://api.example.com:" + port + "/ HTTP/1.1\r\nHost: api.example.com\r\n\r\n", 405},
		"malformed":        {"NONSENSE\r\n\r\n", 400},
		"oversized header": {"CONNECT api.example.com:" + port + " HTTP/1.1\r\nX: " + strings.Repeat("a", requestLimit) + "\r\n\r\n", 400},
	} {
		if _, _, code := connect(t, socket, tc.req); code != tc.code {
			t.Errorf("%s: %d, want %d", name, code, tc.code)
		}
	}
	// Upper case and a trailing dot name the same declared host.
	if _, _, code := connect(t, socket, "CONNECT API.Example.com.:"+port+" HTTP/1.1\r\n\r\n"); code != 200 {
		t.Errorf("normalized host: %d", code)
	}
	time.Sleep(200 * time.Millisecond)
	log := logs.String()
	if !strings.Contains(log, `"verdict":"allow"`) || !strings.Contains(log, `"reason":"not in the egress list"`) || strings.Contains(log, "hello") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestProxyLimits(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	req := "CONNECT api.example.com:" + port + " HTTP/1.1\r\n\r\n"

	pol := DefaultPolicy([]string{"api.example.com:" + port})
	pol.MaxConnections = 1
	socket, _ := startProxy(t, pol, target)
	first, _, code := connect(t, socket, req)
	if code != 200 {
		t.Fatal(code)
	}
	if _, _, code := connect(t, socket, req); code != 503 {
		t.Errorf("second connection: %d", code)
	}
	first.Close()

	pol = DefaultPolicy([]string{"api.example.com:" + port})
	pol.MaxBytes = 10
	socket, _ = startProxy(t, pol, target)
	c, br, code := connect(t, socket, req)
	if code != 200 {
		t.Fatal(code)
	}
	fmt.Fprintln(c, strings.Repeat("x", 20))
	if _, err := br.ReadString('\n'); err == nil {
		t.Error("the byte limit did not close the tunnel")
	}
	if _, _, code := connect(t, socket, req); code != 403 {
		t.Errorf("after the byte limit: %d", code)
	}

	pol = DefaultPolicy([]string{"api.example.com:" + port})
	pol.IdleSeconds = 1
	socket, _ = startProxy(t, pol, target)
	_, br, code = connect(t, socket, req)
	if code != 200 {
		t.Fatal(code)
	}
	start := time.Now()
	if _, err := br.ReadString('\n'); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("idle tunnel: %v after %s", err, time.Since(start))
	}
}

// The forwarder relays a TCP port to the proxy socket.
func TestForward(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	socket, _ := startProxy(t, DefaultPolicy([]string{"api.example.com:" + port}), target)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go Forward(l, socket)
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "CONNECT api.example.com:%s HTTP/1.1\r\n\r\nvia forward\n", port)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	if line, _ := br.ReadString('\n'); line != "VIA FORWARD\n" {
		t.Fatalf("%q", line)
	}
}

// helperExit lets the test binary act as the launched program.
func TestMain(m *testing.M) {
	if code := os.Getenv("EGRESS_HELPER_EXIT"); code != "" {
		var n int
		fmt.Sscanf(code, "%d", &n)
		if addr := os.Getenv("EGRESS_HELPER_DIAL"); addr != "" {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				os.Exit(99)
			}
			c.Close()
		}
		os.Exit(n)
	}
	os.Exit(m.Run())
}

func TestLaunch(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	socket, _ := startProxy(t, DefaultPolicy([]string{"api.example.com:" + port}), target)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	listen := l.Addr().String()
	l.Close()
	var errb bytes.Buffer
	t.Setenv("EGRESS_HELPER_EXIT", "7")
	t.Setenv("EGRESS_HELPER_DIAL", listen)
	if code := Launch(socket, listen, 5*time.Second, []string{os.Args[0]}, &errb); code != 7 {
		t.Fatalf("exit %d, want the program's 7: %s", code, errb.String())
	}
	// No proxy: the launcher gives up before the program starts.
	errb.Reset()
	start := time.Now()
	if code := Launch(filepath.Join(t.TempDir(), "none.sock"), listen, 500*time.Millisecond, []string{os.Args[0]}, &errb); code != LauncherFailed ||
		!strings.Contains(errb.String(), "did not answer") || time.Since(start) > 5*time.Second {
		t.Fatalf("missing proxy: exit %d %q", code, errb.String())
	}
	// A program that cannot start is the launcher's failure.
	errb.Reset()
	if code := Launch(socket, listen, 5*time.Second, []string{filepath.Join(t.TempDir(), "missing")}, &errb); code != LauncherFailed {
		t.Fatalf("missing program: exit %d", code)
	}
}

// A target with anything besides a host and a port is refused, and the log names no host.
func TestMalformedTargetsStayOutOfTheLog(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	socket, logs := startProxy(t, DefaultPolicy([]string{"api.example.com:" + port}), target)
	for _, tgt := range []string{"api.example.com/path?token=secret:443", "user:secret@api.example.com:443", "[fe80::1%eth0]:443",
		strings.Repeat("a", 280) + ".com:443"} {
		if _, _, code := connect(t, socket, "CONNECT "+tgt+" HTTP/1.1\r\n\r\n"); code != 400 {
			t.Errorf("%.40s: %d", tgt, code)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if log := logs.String(); strings.Contains(log, "secret") || strings.Contains(log, `"host"`) {
		t.Fatalf("log:\n%s", log)
	}
}

// A peer that stops reading cannot hold a relay open: the watchdog ends it, blocked
// writes included.
func TestSpliceEndsWhenNobodyReads(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	defer a1.Close()
	defer b1.Close()
	go func() {
		for {
			if _, err := b1.Write(make([]byte, 1024)); err != nil {
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		splice(a2, b2, nil, 300*time.Millisecond, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay outlived its idle time")
	}
}

// Shutdown closes open tunnels and each still writes its log line.
func TestShutdownLogsOpenTunnels(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	tap := netip.MustParseAddrPort(target)
	dir, _ := os.MkdirTemp("", "egr")
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "p.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	logs := &syncBuffer{}
	p := &Proxy{Policy: DefaultPolicy([]string{"api.example.com:" + port}), Log: logs,
		Resolve: func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{tap.Addr()}, nil },
		Allowed: func(a netip.Addr) bool { return a == tap.Addr() }}
	go p.Serve(l)
	c, br, code := connect(t, socket, "CONNECT api.example.com:"+port+" HTTP/1.1\r\n\r\nping\n")
	if code != 200 {
		t.Fatal(code)
	}
	if line, _ := br.ReadString('\n'); line != "PING\n" {
		t.Fatalf("%q", line)
	}
	if err := p.Shutdown(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	_ = c
	if log := logs.String(); !strings.Contains(log, `"verdict":"allow"`) || !strings.Contains(log, `"up":5`) {
		t.Fatalf("log after shutdown:\n%s", log)
	}
}

// Shutdown ends a pending resolution, and the request is still logged.
func TestShutdownCancelsPendingResolution(t *testing.T) {
	dir, _ := os.MkdirTemp("", "egr")
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "p.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	logs := &syncBuffer{}
	started := make(chan struct{})
	p := &Proxy{Policy: DefaultPolicy([]string{"slow.example.com:443"}), Log: logs,
		Resolve: func(ctx context.Context, _ string) ([]netip.Addr, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	go p.Serve(l)
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "CONNECT slow.example.com:443 HTTP/1.1\r\n\r\n")
	<-started
	start := time.Now()
	if err := p.Shutdown(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second || !strings.Contains(logs.String(), "name not resolved") {
		t.Fatalf("after %s, log:\n%s", time.Since(start), logs.String())
	}
}

// Refusals cannot use up the log: every tunnel reserves its line, and once the launch's
// tunnel lines are spent, tunnels are refused instead of opened without a record.
func TestEveryTunnelIsLogged(t *testing.T) {
	oldLog, oldTunnels := logLimit, tunnelLimit
	logLimit, tunnelLimit = 3, 2
	defer func() { logLimit, tunnelLimit = oldLog, oldTunnels }()
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	socket, logs := startProxy(t, DefaultPolicy([]string{"api.example.com:" + port}), target)
	for i := 0; i < 10; i++ {
		connect(t, socket, "NONSENSE\r\n\r\n")
	}
	req := "CONNECT api.example.com:" + port + " HTTP/1.1\r\n\r\n"
	for i := 0; i < 2; i++ {
		c, _, code := connect(t, socket, req)
		if code != 200 {
			t.Fatalf("tunnel %d: %d", i, code)
		}
		c.Close()
	}
	if _, _, code := connect(t, socket, req); code != 403 {
		t.Fatalf("tunnel beyond the limit: %d", code)
	}
	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(logs.String(), `"verdict":"allow"`); n != 2 {
		t.Fatalf("%d tunnel lines, want 2:\n%s", n, logs.String())
	}
}

// blockingLog stalls every write of a tunnel's line.
type blockingLog struct{ release chan struct{} }

func (b blockingLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), `"verdict":"allow"`) {
		<-b.release
	}
	return len(p), nil
}

// A handler that cannot write its line in time makes Shutdown fail, so the proxy exits
// nonzero and aeh reports its log as possibly incomplete.
func TestShutdownFailsWhenALineIsStuck(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	tap := netip.MustParseAddrPort(target)
	dir, _ := os.MkdirTemp("", "egr")
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "p.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("no Unix sockets here: %v", err)
	}
	stuck := blockingLog{release: make(chan struct{})}
	defer close(stuck.release)
	p := &Proxy{Policy: DefaultPolicy([]string{"api.example.com:" + port}), Log: stuck,
		Resolve: func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{tap.Addr()}, nil },
		Allowed: func(a netip.Addr) bool { return a == tap.Addr() }}
	go p.Serve(l)
	if _, _, code := connect(t, socket, "CONNECT api.example.com:"+port+" HTTP/1.1\r\n\r\n"); code != 200 {
		t.Fatal(code)
	}
	if err := p.Shutdown(500 * time.Millisecond); err == nil {
		t.Fatal("a stuck log line was not reported")
	}
}

// A connection that closes before sending anything (the launcher's readiness probe) is
// not logged; one that sends a partial request still is.
func TestEmptyConnectionIsNotLogged(t *testing.T) {
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	socket, logs := startProxy(t, DefaultPolicy([]string{"api.example.com:" + port}), target)
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("CONN"))
	c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(logs.String(), "malformed request") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), "malformed request") {
		t.Fatalf("want only the partial request logged:\n%s", logs.String())
	}
}
