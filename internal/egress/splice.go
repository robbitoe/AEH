package egress

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// splice copies a to b and b to a until both directions end, and returns the bytes each
// way. pending is sent to b first. A watchdog closes both connections once no byte has
// arrived from either side for idle, which also ends writes blocked on a peer that
// stopped reading. count, when not nil, accounts each chunk read; a false answer closes
// both connections.
func splice(a, b net.Conn, pending []byte, idle time.Duration, count func(int64) bool) (aToB, bToA int64) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { _ = a.Close(); _ = b.Close() }) }
	defer closeBoth()
	if count == nil {
		count = func(int64) bool { return true }
	}
	if len(pending) > 0 {
		if !count(int64(len(pending))) {
			return 0, 0
		}
		_ = b.SetWriteDeadline(time.Now().Add(idle))
		if _, err := b.Write(pending); err != nil {
			return 0, 0
		}
		_ = b.SetWriteDeadline(time.Time{})
		aToB = int64(len(pending))
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	stop := make(chan struct{})
	go func() {
		tick := idle / 10
		if tick > time.Second {
			tick = time.Second
		}
		if tick < 10*time.Millisecond {
			tick = 10 * time.Millisecond
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if time.Since(time.Unix(0, last.Load())) >= idle {
					closeBoth()
					return
				}
			}
		}
	}()
	pipe := func(dst, src net.Conn, n *int64) {
		buf := make([]byte, 32<<10)
		for {
			k, err := src.Read(buf)
			if k > 0 {
				last.Store(time.Now().UnixNano())
				if !count(int64(k)) {
					closeBoth()
					return
				}
				if _, werr := dst.Write(buf[:k]); werr != nil {
					closeBoth()
					return
				}
				*n += int64(k)
			}
			if err == io.EOF {
				closeWrite(dst)
				return
			}
			if err != nil {
				closeBoth()
				return
			}
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pipe(a, b, &bToA)
	}()
	pipe(b, a, &aToB)
	wg.Wait()
	close(stop)
	return aToB, bToA
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
