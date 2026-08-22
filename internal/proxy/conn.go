package proxy

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ipConnTracker counts how many active TCP connections exist per client IP.
// It is shared across all backends so the per-IP cap is enforced globally.
type ipConnTracker struct {
	mu   sync.Mutex
	live map[string]*atomic.Int64
}

func newIPConnTracker() *ipConnTracker {
	return &ipConnTracker{live: make(map[string]*atomic.Int64)}
}

func (t *ipConnTracker) inc(ip string) {
	t.mu.Lock()
	m, ok := t.live[ip]
	if !ok {
		m = &atomic.Int64{}
		t.live[ip] = m
	}
	_ = m.Add(1)
	t.mu.Unlock()
}

func (t *ipConnTracker) dec(ip string) {
	t.mu.Lock()
	if m, ok := t.live[ip]; ok {
		if m.Add(-1) <= 0 {
			delete(t.live, ip)
		}
	}
	t.mu.Unlock()
}

func (t *ipConnTracker) count(ip string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m, ok := t.live[ip]; ok {
		return int(m.Load())
	}
	return 0
}

// countConn wraps a net.Conn, counts bytes written to it and refreshes the
// idle deadline on every read/write so a quiet half of the stream does not
// cause a premature close of the active half.
type countConn struct {
	net.Conn
	wgt  *atomic.Int64
	idle time.Duration
}

func newCountConn(c net.Conn, wgt *atomic.Int64, idle time.Duration) *countConn {
	return &countConn{Conn: c, wgt: wgt, idle: idle}
}

func (c *countConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c *countConn) Write(p []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.wgt.Add(int64(n))
	}
	return n, err
}

// pipe copies src -> dst until either side closes, counting nothing itself
// (counting is handled by the readers/writers passed in).
func pipe(dst, src net.Conn, bufSize int) {
	if bufSize <= 0 {
		bufSize = 32 * 1024
	}
	buf := make([]byte, bufSize)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}
