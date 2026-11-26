package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// requestLimit bounds a CONNECT request line and headers.
	requestLimit = 16 << 10
)

// Limits of the proxy's log (variables so tests can lower them). Refusals are logged up
// to logLimit lines; the last one says that logging of refusals stopped. A launch opens
// at most tunnelLimit tunnels, each of which reserves its line before it is admitted, so
// every tunnel is recorded; beyond the limit tunnels are refused.
var (
	logLimit    = 10000
	tunnelLimit = 10000
)

// Proxy serves CONNECT requests on a listener (a Unix socket in the sandbox). It resolves
// names itself and connects only to a public address it validated, for a declared pair.
type Proxy struct {
	Policy Policy
	// Resolve returns a host's addresses. Nil: the system resolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Allowed judges an address. Nil: Public. Tests replace it to reach a local server.
	Allowed func(netip.Addr) bool
	// Log receives one JSON line per request: host, port, bytes and verdict only.
	Log io.Writer

	allow    map[string]bool
	slots    chan struct{}
	refusals chan struct{}
	bytes    atomic.Int64
	logMu    sync.Mutex
	logged   int
	tunnels  int
	mu       sync.Mutex
	open     map[net.Conn]bool
	listener net.Listener
	closing  bool
	handlers sync.WaitGroup
	ctx      context.Context // ends with Shutdown: pending resolutions and dials stop
	cancel   context.CancelFunc
}

// logEntry is one line of the proxy's log. It holds no path, header or content.
type logEntry struct {
	Time    string `json:"time"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Up      int64  `json:"up,omitempty"`
	Down    int64  `json:"down,omitempty"`
}

// reserveTunnel takes one of the launch's tunnel log lines, or reports none is left.
func (p *Proxy) reserveTunnel() bool {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	if p.tunnels >= tunnelLimit {
		return false
	}
	p.tunnels++
	return true
}

// logTunnel writes the line a tunnel reserved; it is never dropped.
func (p *Proxy) logTunnel(e logEntry) {
	if p.Log == nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	e.Time = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(e)
	_, _ = p.Log.Write(append(b, '\n'))
}

// log writes a refusal, within logLimit.
func (p *Proxy) log(e logEntry) {
	if p.Log == nil {
		return
	}
	p.logMu.Lock()
	defer p.logMu.Unlock()
	if p.logged > logLimit {
		return
	}
	p.logged++
	if p.logged > logLimit {
		e = logEntry{Verdict: "log-limit", Reason: "further requests are not logged"}
	}
	e.Time = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(e)
	_, _ = p.Log.Write(append(b, '\n'))
}

// Serve accepts connections until the listener fails or Shutdown closes it.
func (p *Proxy) Serve(l net.Listener) error {
	if err := p.Policy.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	p.allow = map[string]bool{}
	for _, a := range p.Policy.Allow {
		p.allow[a] = true
	}
	p.slots = make(chan struct{}, p.Policy.MaxConnections)
	p.refusals = make(chan struct{}, 16)
	p.open = map[net.Conn]bool{}
	p.listener = l
	if p.ctx == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
	}
	closing := p.closing
	p.mu.Unlock()
	if closing {
		_ = l.Close()
		return net.ErrClosed
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		select {
		case p.slots <- struct{}{}:
			if !p.trackHandler(c) {
				<-p.slots
				_ = c.Close()
				continue
			}
			go func() {
				defer p.handlers.Done()
				defer func() { <-p.slots }()
				defer p.untrack(c)
				p.handle(c)
			}()
		default:
			p.log(logEntry{Verdict: "deny", Reason: "too many connections"})
			select {
			case p.refusals <- struct{}{}:
				// The request is read before the refusal: closing a socket with unread
				// input resets it on some systems, and the client would never see the
				// answer. A few refusals run at a time; beyond that the socket just closes.
				go func() {
					defer func() { <-p.refusals }()
					_ = c.SetDeadline(time.Now().Add(2 * time.Second))
					_, _ = http.ReadRequest(bufio.NewReader(io.LimitReader(c, requestLimit)))
					reply(c, http.StatusServiceUnavailable)
					_ = c.Close()
				}()
			default:
				_ = c.Close()
			}
		}
	}
}

// track registers an open connection so Shutdown can close it; it refuses once the
// proxy is closing.
func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return false
	}
	p.open[c] = true
	return true
}

// trackHandler registers a client connection and its handler together, under the lock
// Shutdown takes, so Shutdown either refuses the handler or waits for it.
func (p *Proxy) trackHandler(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return false
	}
	p.open[c] = true
	p.handlers.Add(1)
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.open, c)
}

// Shutdown stops accepting, closes every open connection and waits up to wait for the
// handlers to write their final log lines, with the bytes each tunnel carried.
func (p *Proxy) Shutdown(wait time.Duration) {
	p.mu.Lock()
	p.closing = true
	if p.cancel != nil {
		p.cancel()
	}
	if p.listener != nil {
		_ = p.listener.Close()
	}
	for c := range p.open {
		_ = c.Close()
	}
	p.mu.Unlock()
	done := make(chan struct{})
	go func() {
		p.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wait):
	}
}

func reply(c net.Conn, code int) {
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, "HTTP/1.1 "+strconv.Itoa(code)+" "+http.StatusText(code)+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

func (p *Proxy) handle(c net.Conn) {
	defer c.Close()
	// One deadline covers reading the request, resolving the name and connecting.
	deadline := time.Now().Add(time.Duration(p.Policy.ConnectSeconds) * time.Second)
	_ = c.SetDeadline(deadline)
	br := bufio.NewReaderSize(io.LimitReader(c, requestLimit), 4096)
	req, err := http.ReadRequest(br)
	if err != nil {
		p.log(logEntry{Verdict: "deny", Reason: "malformed request"})
		reply(c, http.StatusBadRequest)
		return
	}
	if req.Method != http.MethodConnect {
		p.log(logEntry{Verdict: "deny", Reason: "only CONNECT is allowed"})
		reply(c, http.StatusMethodNotAllowed)
		return
	}
	host, port, key, err := normalizeTarget(req.RequestURI)
	if err != nil {
		// Nothing of an invalid target is logged: it could carry anything.
		p.log(logEntry{Verdict: "deny", Reason: "malformed target"})
		reply(c, http.StatusBadRequest)
		return
	}
	entry := logEntry{Host: host, Port: int(port)}
	deny := func(code int, reason string) {
		entry.Verdict, entry.Reason = "deny", reason
		p.log(entry)
		reply(c, code)
	}
	if !p.allow[key] {
		deny(http.StatusForbidden, "not in the egress list")
		return
	}
	if p.bytes.Load() >= p.Policy.MaxBytes {
		deny(http.StatusForbidden, "byte limit reached")
		return
	}
	if !p.reserveTunnel() {
		deny(http.StatusForbidden, "tunnel limit reached")
		return
	}
	// The tunnel's line is reserved: from here every path writes it once.
	defer func() { p.logTunnel(entry) }()
	refuse := func(code int, reason string) {
		entry.Verdict, entry.Reason = "deny", reason
		reply(c, code)
	}
	ctx, cancel := context.WithDeadline(p.ctx, deadline)
	defer cancel()
	addrs, err := p.resolve(ctx, host)
	if err != nil {
		refuse(http.StatusBadGateway, "name not resolved")
		return
	}
	allowed := p.Allowed
	if allowed == nil {
		allowed = Public
	}
	var up net.Conn
	refused, tried := false, false
	for _, a := range addrs {
		a = a.Unmap()
		if !allowed(a) {
			refused = true
			continue
		}
		tried = true
		var d net.Dialer
		if up, err = d.DialContext(ctx, "tcp", netip.AddrPortFrom(a, port).String()); err == nil {
			break
		}
		up = nil
	}
	if up == nil {
		if refused && !tried {
			refuse(http.StatusForbidden, "no public address")
		} else {
			refuse(http.StatusBadGateway, "connection failed")
		}
		return
	}
	defer up.Close()
	entry.Verdict = "allow"
	if !p.track(up) {
		entry.Reason = "proxy shut down before the tunnel opened"
		return
	}
	defer p.untrack(up)
	_ = c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		entry.Reason = "client gone before the tunnel opened"
		return
	}
	// Bytes the client sent after the request are already buffered.
	pending := make([]byte, br.Buffered())
	_, _ = io.ReadFull(br, pending)
	entry.Up, entry.Down = splice(c, up, pending, time.Duration(p.Policy.IdleSeconds)*time.Second, p.count)
	if p.bytes.Load() >= p.Policy.MaxBytes {
		entry.Reason = "byte limit reached"
	}
}

func (p *Proxy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	if p.Resolve != nil {
		return p.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// count adds n bytes to the launch's total and reports whether it stays within the limit.
func (p *Proxy) count(n int64) bool {
	return p.bytes.Add(n) <= p.Policy.MaxBytes
}
