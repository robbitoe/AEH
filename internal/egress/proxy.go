package egress

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	// logLimit bounds the proxy's log lines; the last one says that logging stopped.
	logLimit = 10000
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

	allow  map[string]bool
	slots  chan struct{}
	bytes  atomic.Int64
	logMu  sync.Mutex
	logged int
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

// Serve accepts connections until the listener fails.
func (p *Proxy) Serve(l net.Listener) error {
	if err := p.Policy.Validate(); err != nil {
		return err
	}
	p.allow = map[string]bool{}
	for _, a := range p.Policy.Allow {
		p.allow[a] = true
	}
	p.slots = make(chan struct{}, p.Policy.MaxConnections)
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		select {
		case p.slots <- struct{}{}:
			go func() {
				defer func() { <-p.slots }()
				p.handle(c)
			}()
		default:
			p.log(logEntry{Verdict: "deny", Reason: "too many connections"})
			reply(c, http.StatusServiceUnavailable)
			_ = c.Close()
		}
	}
}

func reply(c net.Conn, code int) {
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, "HTTP/1.1 "+strconv.Itoa(code)+" "+http.StatusText(code)+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
}

func (p *Proxy) handle(c net.Conn) {
	defer c.Close()
	connect := time.Duration(p.Policy.ConnectSeconds) * time.Second
	_ = c.SetDeadline(time.Now().Add(connect))
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
	ctx, cancel := context.WithTimeout(context.Background(), connect)
	defer cancel()
	addrs, err := p.resolve(ctx, host)
	if err != nil {
		deny(http.StatusBadGateway, "name not resolved")
		return
	}
	allowed := p.Allowed
	if allowed == nil {
		allowed = Public
	}
	var up net.Conn
	refused := false
	for _, a := range addrs {
		a = a.Unmap()
		if !allowed(a) {
			refused = true
			continue
		}
		var d net.Dialer
		if up, err = d.DialContext(ctx, "tcp", netip.AddrPortFrom(a, port).String()); err == nil {
			break
		}
	}
	if up == nil {
		if refused && err == nil {
			deny(http.StatusForbidden, "no public address")
		} else {
			deny(http.StatusBadGateway, "connection failed")
		}
		return
	}
	defer up.Close()
	_ = c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	// Bytes the client sent after the request are already buffered.
	pending := make([]byte, br.Buffered())
	_, _ = io.ReadFull(br, pending)
	entry.Up, entry.Down = p.tunnel(c, up, pending)
	entry.Verdict = "allow"
	if p.bytes.Load() >= p.Policy.MaxBytes {
		entry.Reason = "byte limit reached"
	}
	p.log(entry)
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

// tunnel copies both ways until both directions end, the tunnel is idle for the policy's
// time or the launch's byte limit is reached.
func (p *Proxy) tunnel(client, up net.Conn, pending []byte) (sent, received int64) {
	idle := time.Duration(p.Policy.IdleSeconds) * time.Second
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var once sync.Once
	closeBoth := func() { once.Do(func() { _ = client.Close(); _ = up.Close() }) }
	if len(pending) > 0 {
		if !p.count(int64(len(pending))) {
			closeBoth()
			return 0, 0
		}
		if _, err := up.Write(pending); err != nil {
			closeBoth()
			return 0, 0
		}
		sent = int64(len(pending))
	}
	pipe := func(dst, src net.Conn, n *int64) {
		buf := make([]byte, 32<<10)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
			k, err := src.Read(buf)
			if k > 0 {
				last.Store(time.Now().UnixNano())
				if !p.count(int64(k)) {
					closeBoth()
					return
				}
				if _, werr := dst.Write(buf[:k]); werr != nil {
					closeBoth()
					return
				}
				*n += int64(k)
			}
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() && time.Since(time.Unix(0, last.Load())) < idle {
					continue // the other direction is active
				}
				if errors.Is(err, io.EOF) {
					closeWrite(dst)
					return
				}
				closeBoth()
				return
			}
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe(client, up, &received)
	}()
	pipe(up, client, &sent)
	wg.Wait()
	closeBoth()
	return sent, received
}

// count adds n bytes to the launch's total and reports whether it stays within the limit.
func (p *Proxy) count(n int64) bool {
	return p.bytes.Add(n) <= p.Policy.MaxBytes
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
