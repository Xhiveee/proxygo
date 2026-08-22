package proxy

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"proxygo/internal/config"
	"proxygo/internal/logging"
	"proxygo/internal/metrics"
	"proxygo/internal/model"
)

// UDPForwarder forwards datagrams between clients and one backend UDP
// address, keyed by client IP:port. Sessions are created lazily and expire
// after UDPSessionTimeout of inactivity.
//
// Each session owns an unconnected upstream socket bound to a new ephemeral
// port. Using an unconnected socket (instead of net.DialUDP) avoids Windows
// quirks where a connected socket fails to receive the reply on loopback, and
// keeps the mapping clean: a reply arriving on an ephemeral port belongs
// exactly to one client.
type UDPForwarder struct {
	model *model.Backend
	cfg   *config.Config
	log   *logging.Logger
	ms    *metrics.Metrics
	notify Notifier

	conn    *net.UDPConn // listening socket (client-facing)
	backend *net.UDPAddr // resolved backend address

	banFn func(string) (bool, error)

	mu       sync.Mutex
	sessions map[string]*udpSession

	bytes    atomic.Int64
	packets  atomic.Int64
	lastDDoS atomic.Int64

	ctx chan struct{}
	wg  sync.WaitGroup
}

type udpSession struct {
	client    *net.UDPAddr
	upstream  *net.UDPConn
	upAddr    *net.UDPAddr // upstream socket local address (source of replies)
	lastSeen  atomic.Int64 // unix nanos
	closeOnce sync.Once
}

func newUDPForwarder(m *model.Backend, cfg *config.Config, log *logging.Logger,
	ms *metrics.Metrics, notify Notifier) (*UDPForwarder, error) {
	dup, err := net.ResolveUDPAddr("udp", m.BackendUDP)
	if err != nil {
		return nil, err
	}
	return &UDPForwarder{
		model:    m,
		cfg:      cfg,
		log:      log,
		ms:       ms,
		notify:   notify,
		backend:  dup,
		sessions: make(map[string]*udpSession),
		ctx:      make(chan struct{}),
	}, nil
}

// SetBanFn wires the storage-backed ban lookup into the forwarder.
func (u *UDPForwarder) SetBanFn(fn func(string) (bool, error)) { u.banFn = fn }

// Start binds the listening socket and begins the read loop.
func (u *UDPForwarder) Start() error {
	addr := net.JoinHostPort(u.cfg.Proxy.ListenInterface, itoa(u.model.UDPPort))
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	u.conn = pc.(*net.UDPConn)
	u.wg.Add(1)
	go u.run()
	u.log.Info("udp backend up", "backend", u.model.Name, "listen", addr, "target", u.model.BackendUDP)
	return nil
}

// Close stops the forwarder and all sessions.
func (u *UDPForwarder) Close() {
	select {
	case <-u.ctx:
		return
	default:
		close(u.ctx)
	}
	if u.conn != nil {
		_ = u.conn.Close()
	}
	u.mu.Lock()
	for _, s := range u.sessions {
		s.closeOnce.Do(func() { _ = s.upstream.Close() })
	}
	u.sessions = make(map[string]*udpSession)
	u.mu.Unlock()
	u.wg.Wait()
}

func (u *UDPForwarder) run() {
	defer u.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, client, err := u.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-u.ctx:
				return
			default:
			}
			u.ms.ErrorsTotal.Add(1)
			u.log.Warn("udp read error", "backend", u.model.Name, "err", err)
			continue
		}
		if n <= 0 {
			continue
		}
		u.handlePacket(buf[:n], client)
	}
}

func (u *UDPForwarder) handlePacket(data []byte, client *net.UDPAddr) {
	ip := client.IP.String()

	if banned, err := u.ban(ip); err == nil && banned {
		u.ms.ErrRateLimit.Add(1)
		return
	}
	if max := u.cfg.Security.MaxUDPPacketsPerIPSec; max > 0 &&
		u.ms.UDPPacketsPerSec(ip) > int64(max) {
		u.ms.ErrRateLimit.Add(1)
		return // silently drop; UDP has no reliable delivery contract
	}
	if max := u.cfg.Security.MaxUDPPacketsPerMin; max > 0 &&
		u.ms.UDPPacketsPerMin(ip) > int64(max) {
		u.notifyDDoS(ip)
	}

	sess := u.getOrCreate(client)
	if sess == nil {
		return
	}
	u.ms.IncUDPPacket(ip)
	sess.lastSeen.Store(time.Now().UnixNano())
	u.packets.Add(1)
	u.bytes.Add(int64(len(data)))

	// Bind the upstream source to its ephemeral local address so replies can
	// be written back to the right client.
	if _, err := sess.upstream.WriteToUDP(data, u.backend); err != nil {
		u.ms.ErrWrite.Add(1)
		u.dropSession(client)
	}
}

// getOrCreate returns the session for the client, creating it if absent.
func (u *UDPForwarder) getOrCreate(client *net.UDPAddr) *udpSession {
	key := client.String()
	u.mu.Lock()
	defer u.mu.Unlock()
	if s, ok := u.sessions[key]; ok {
		return s
	}
	up, err := net.ListenUDP("udp", &net.UDPAddr{IP: u.bindIP()})
	if err != nil {
		u.ms.ErrDial.Add(1)
		u.log.Warn("udp session socket failed", "backend", u.model.Name, "client", key, "err", err)
		return nil
	}
	s := &udpSession{
		client:   client,
		upstream: up,
		upAddr:   up.LocalAddr().(*net.UDPAddr),
	}
	s.lastSeen.Store(time.Now().UnixNano())
	u.sessions[key] = s
	u.wg.Add(1)
	go s.readLoop(u, u.cfg.Proxy.UDPSession)
	return s
}

func (u *UDPForwarder) bindIP() net.IP {
	ip := net.ParseIP(u.cfg.Proxy.ListenInterface)
	if ip != nil && !ip.IsUnspecified() {
		return ip
	}
	// Bind to the wildcard so routing picks an outbound source.
	return nil
}

func (u *UDPForwarder) dropSession(client *net.UDPAddr) {
	key := client.String()
	u.mu.Lock()
	s, ok := u.sessions[key]
	if ok {
		delete(u.sessions, key)
	}
	u.mu.Unlock()
	if ok {
		s.closeOnce.Do(func() { _ = s.upstream.Close() })
	}
}

func (u *UDPForwarder) ban(ip string) (bool, error) {
	if u.banFn != nil {
		return u.banFn(ip)
	}
	return false, nil
}

func (u *UDPForwarder) notifyDDoS(ip string) {
	now := time.Now().Unix()
	last := u.lastDDoS.Load()
	if now-last < 60 {
		return
	}
	if u.lastDDoS.CompareAndSwap(last, now) {
		u.ms.ErrRateLimit.Add(1)
		u.log.Warn("ddos suspicion", "backend", u.model.Name, "ip", ip, "proto", "udp")
		u.notify.Notify("рџ”Ґ DDoS detect: " + ip + " (proto=udp) on " + u.model.Name)
	}
}

// readLoop forwards replies from the upstream to the client until idle.
func (s *udpSession) readLoop(u *UDPForwarder, timeout time.Duration) {
	if timeout <= 0 {
		timeout = time.Minute
	}
	defer u.wg.Done()
	buf := make([]byte, 65535)
	for {
		_ = s.upstream.SetReadDeadline(time.Now().Add(timeout))
		n, _, err := s.upstream.ReadFromUDP(buf)
		if n > 0 {
			if _, werr := u.conn.WriteToUDP(buf[:n], s.client); werr != nil {
				return
			}
			s.lastSeen.Store(time.Now().UnixNano())
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if time.Since(time.Unix(0, s.lastSeen.Load())) > timeout {
					u.dropSession(s.client)
					return
				}
				continue
			}
			u.dropSession(s.client)
			return
		}
	}
}
