// Package proxy implements the hybrid TCP/UDP forwarding layer for the
// MC hybrid proxy. TCP connections get a Proxy Protocol v2 header prepended
// so the backend Java agent can recover the real client IP. UDP is forwarded
// transparently with session mapping keyed by client IP:port.
package proxy

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"proxygo/internal/config"
	"proxygo/internal/logging"
	"proxygo/internal/metrics"
	"proxygo/internal/model"
	"proxygo/pkg/ppv2"
)

// Storage is the persistence surface the proxy layer needs.
type Storage interface {
	IsBanned(ip string) (bool, error)
	RecordStat(p model.StatPoint) error
}

// Notifier receives asynchronous high-signal alerts.
type Notifier interface {
	Notify(text string)
}

// ErrNotFound is returned when a backend name does not resolve.
var ErrNotFound = errors.New("proxy: backend not found")

// Backend is a live, running proxied destination (one TCP + optional UDP).
type Backend struct {
	model *model.Backend

	cfg     *config.Config
	log     *logging.Logger
	metrics *metrics.Metrics
	store   Storage
	notify  Notifier
	tracker *ipConnTracker

	mu          sync.Mutex
	tcpListener net.Listener
	udpFwd      *UDPForwarder

	connsMu sync.Mutex
	conns   map[net.Conn]struct{}

	started   time.Time
	active    atomic.Int64
	tcpIn     atomic.Int64 // bytes client -> backend
	tcpOut    atomic.Int64 // bytes backend -> client
	udpBytes  atomic.Int64
	udpPkts   atomic.Int64
	connTotal atomic.Int64
	dialFail  atomic.Int64
	lastDDOS  atomic.Int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newBackend builds a runtime backend without starting listeners.
func newBackend(m *model.Backend, cfg *config.Config, log *logging.Logger, ms *metrics.Metrics,
	store Storage, notify Notifier, tracker *ipConnTracker) *Backend {
	ctx, cancel := context.WithCancel(context.Background())
	return &Backend{
		model:   m,
		cfg:     cfg,
		log:     log,
		metrics: ms,
		store:   store,
		notify:  notify,
		tracker: tracker,
		started: time.Now(),
		ctx:     ctx,
		cancel:  cancel,
		conns:   make(map[net.Conn]struct{}),
	}
}

// Name returns the backend name.
func (b *Backend) Name() string { return b.model.Name }

// Model returns a snapshot of the persisted backend record.
func (b *Backend) Model() model.Backend { return *b.model }

// StartedAt returns when the backend instance was created.
func (b *Backend) StartedAt() time.Time { return b.started }

// ActiveConns returns the number of live TCP connections.
func (b *Backend) ActiveConns() int64 { return b.active.Load() }

// Stats returns cumulative counters.
func (b *Backend) Stats() (tcpIn, tcpOut, udpBytes, udpPkts, conns int64) {
	return b.tcpIn.Load(), b.tcpOut.Load(), b.udpBytes.Load(), b.udpPkts.Load(), b.connTotal.Load()
}

// peakConnReturn is used by drain awaiting.
func (b *Backend) isIdle() bool { return b.active.Load() == 0 }

// start opens listeners. TCP is always brought up; UDP only when configured.
func (b *Backend) start() error {
	b.wg.Add(1)
	go b.statsFlusher()
	if err := b.startTCP(); err != nil {
		return err
	}
	if b.model.UDPEnabled {
		if err := b.startUDP(); err != nil {
			b.log.Error("udp start failed", "backend", b.Name(), "err", err)
		}
	}
	return nil
}

// statsFlusher periodically snapshots byte counters into the hourly stats
// row. Deltas are computed against the previous snapshot so repeated flushes
// never over-count.
func (b *Backend) statsFlusher() {
	defer b.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var lastIn, lastOut, lastUDP, lastPkts, lastConns int64
	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			in, out, udp, pkts, conns := b.Stats()
			di, do := in-lastIn, out-lastOut
			du, dp := udp-lastUDP, pkts-lastPkts
			dc := conns - lastConns
			if di == 0 && do == 0 && du == 0 && dp == 0 && dc == 0 {
				continue
			}
			_ = b.store.RecordStat(model.StatPoint{
				BackendID:  b.model.ID,
				Hour:       time.Now().Truncate(time.Hour).Unix(),
				TCPBytes:   di + do,
				TCPConns:   dc,
				UDPBytes:   du,
				UDPPackets: dp,
			})
			lastIn, lastOut, lastUDP, lastPkts, lastConns = in, out, udp, pkts, conns
		}
	}
}

// stop is an immediate forced shutdown (used by /restart and boot failures).
func (b *Backend) stop() {
	b.shutdown(0)
}

// shutdown stops accepting new work and drains active connections. If the
// active connections do not settle within `grace`, they are force-closed.
func (b *Backend) shutdown(grace time.Duration) {
	b.cancel()
	if b.tcpListener != nil {
		_ = b.tcpListener.Close()
	}
	if b.udpFwd != nil {
		b.udpFwd.Close()
	}
	deadline := time.Now().Add(grace)
	for b.active.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	b.forceCloseConns()
	b.wg.Wait()
}

// forceCloseConns closes any remaining live sockets.
func (b *Backend) forceCloseConns() {
	b.connsMu.Lock()
	defer b.connsMu.Unlock()
	for c := range b.conns {
		_ = c.Close()
	}
	b.conns = make(map[net.Conn]struct{})
}

// trackConn registers a live socket for force-close on drain.
func (b *Backend) trackConn(c net.Conn) {
	b.connsMu.Lock()
	b.conns[c] = struct{}{}
	b.connsMu.Unlock()
}

// untrackConn removes a socket once its copy goroutine has returned.
func (b *Backend) untrackConn(c net.Conn) {
	b.connsMu.Lock()
	delete(b.conns, c)
	b.connsMu.Unlock()
}

// restart re-opens the listeners after a shutdown. Used for /restart.
func (b *Backend) restart(drain bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if drain {
		b.shutdown(30 * time.Second)
	} else {
		b.shutdown(0)
	}
	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.started = time.Now()
	b.udpFwd = nil
	b.dialFail.Store(0)
	return b.start()
}

// ---------- TCP ------------------------------------------------------------

func (b *Backend) startTCP() error {
	addr := net.JoinHostPort(b.cfg.Proxy.ListenInterface, itoa(b.model.ListenPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	b.tcpListener = ln
	b.wg.Add(1)
	go b.acceptLoop(ln)
	b.log.Info("tcp backend up", "backend", b.Name(), "listen", addr, "target", b.model.BackendTCP)
	return nil
}

// ---------- UDP ------------------------------------------------------------

func (b *Backend) startUDP() error {
	fwd, err := newUDPForwarder(b.model, b.cfg, b.log, b.metrics, b.notify)
	if err != nil {
		return err
	}
	fwd.SetBanFn(b.store.IsBanned)
	if err := fwd.Start(); err != nil {
		return err
	}
	b.udpFwd = fwd
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (b *Backend) acceptLoop(ln net.Listener) {
	defer b.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if b.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			b.log.Error("tcp accept error", "backend", b.Name(), "err", err)
			b.metrics.ErrorsTotal.Add(1)
			continue
		}
		b.handleConn(conn)
	}
}

func (b *Backend) handleConn(conn net.Conn) {
	raddr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		conn.Close()
		return
	}
	clientIP := raddr.IP.String()

	// Ban check.
	if banned, err := b.store.IsBanned(clientIP); err == nil && banned {
		b.metrics.ErrRateLimit.Add(1)
		b.log.Info("banned client rejected", "backend", b.Name(), "ip", clientIP)
		conn.Close()
		return
	}
	// Per-IP concurrent limit.
	if max := b.cfg.Security.MaxTCPConnsPerIP; max > 0 && b.tracker.count(clientIP) >= max {
		b.metrics.ErrRateLimit.Add(1)
		b.log.Info("max conns per ip exceeded", "backend", b.Name(), "ip", clientIP)
		conn.Close()
		return
	}
	// DDoS detection (per minute).
	if max := b.cfg.Security.MaxTCPConnsPerMin; max > 0 &&
		b.metrics.ConnectionsPerMin(clientIP) > int64(max) {
		b.notifyDDoS(clientIP, "tcp")
	}

	// Dial backend.
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
	backend, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", b.model.BackendTCP)
	cancel()
	if err != nil {
		b.metrics.ErrDial.Add(1)
		if n := b.dialFail.Add(1); n == cfgDialAlertThreshold {
			b.notify.Notify("⚠️ backend dial failed x3: " + b.Name() + " -> " + b.model.BackendTCP)
		}
		b.log.Warn("dial backend failed", "backend", b.Name(), "client", clientIP, "err", err)
		conn.Close()
		return
	}
	b.dialFail.Store(0)

	// Build and send the PPv2 header before any data.
	hdrBytes, herr := b.buildPPv2(raddr)
	if herr != nil {
		b.metrics.ErrWrite.Add(1)
		b.log.Error("ppv2 build failed", "backend", b.Name(), "err", herr)
		conn.Close()
		backend.Close()
		return
	}
	if _, err := backend.Write(hdrBytes); err != nil {
		b.metrics.ErrBackendUp.Add(1)
		b.log.Warn("write ppv2 to backend failed", "backend", b.Name(), "client", clientIP, "err", err)
		conn.Close()
		backend.Close()
		return
	}

	b.active.Add(1)
	b.connTotal.Add(1)
	b.metrics.IncConnection(clientIP)
	b.tracker.inc(clientIP)
	b.trackConn(conn)
	b.trackConn(backend)

	cw := newCountConn(conn, &b.tcpOut, b.cfg.Proxy.DefaultIdle)
	bw := newCountConn(backend, &b.tcpIn, b.cfg.Proxy.DefaultIdle)

	// closeBoth runs exactly once, when either copy direction finishes, and
	// releases every reference so the live per-IP count and force-close set
	// stay accurate over the whole connection lifetime.
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = cw.Close()
			_ = bw.Close()
			b.untrackConn(conn)
			b.untrackConn(backend)
			b.tracker.dec(clientIP)
			b.active.Add(-1)
		})
	}

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer closeBoth()
		pipe(cw, bw, b.cfg.Proxy.BufferSize) // client -> backend
	}()
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer closeBoth()
		pipe(bw, cw, b.cfg.Proxy.BufferSize) // backend -> client
	}()
}

// buildPPv2 produces the PROXY v2 header for the given client address.
func (b *Backend) buildPPv2(raddr *net.TCPAddr) ([]byte, error) {
	host, portStr := splitHostPort(b.model.BackendTCP)
	dport, _ := parsePort(portStr)
	hdr := ppv2.Header{
		Command:    ppv2.CommandProxy,
		Family:     familyFor(raddr.IP),
		Protocol:   ppv2.ProtocolStream,
		SourceAddr: raddr.IP,
		SourcePort: uint16(raddr.Port),
		DestAddr:   net.ParseIP(host),
		DestPort:   dport,
	}
	if !hdr.Valid() {
		// fall back to unspecified family (no address block) if dest/host missing
		hdr.Family = ppv2.FamilyUnspec
		hdr.SourceAddr, hdr.DestAddr, hdr.SourcePort, hdr.DestPort = nil, nil, 0, 0
	}
	return hdr.Marshal()
}

func (b *Backend) notifyDDoS(ip, proto string) {
	now := time.Now().Unix()
	last := b.lastDDOS.Load()
	if now-last < 60 {
		return
	}
	if b.lastDDOS.CompareAndSwap(last, now) {
		b.metrics.ErrRateLimit.Add(1)
		b.log.Warn("ddos suspicion", "backend", b.Name(), "ip", ip, "proto", proto)
		b.notify.Notify("🔥 DDoS detect: " + ip + " (proto=" + proto + ") on " + b.Name())
	}
}

// cfgDialAlertThreshold is the number of consecutive dial failures before
// alerting once. Not configurable to keep the surface small.
const cfgDialAlertThreshold = 3

func familyFor(ip net.IP) byte {
	if ip.To4() != nil {
		return ppv2.FamilyINET
	}
	return ppv2.FamilyINET6
}

func splitHostPort(hostport string) (host, port string) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, "0"
	}
	return host, port
}

func parsePort(s string) (uint16, error) {
	var n uint16
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("invalid port")
		}
		n = n*10 + uint16(c-'0')
	}
	return n, nil
}
