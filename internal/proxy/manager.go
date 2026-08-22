package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"proxygo/internal/config"
	"proxygo/internal/logging"
	"proxygo/internal/metrics"
	"proxygo/internal/model"
)

// Manager owns the set of running backends and their lifecycle. It is the
// facade used by the Telegram bot for all admin operations.
type Manager struct {
	cfg     *config.Config
	log     *logging.Logger
	ms      *metrics.Metrics
	store   StoragePersist
	notify  Notifier
	tracker *ipConnTracker

	mu       sync.Mutex
	backends map[string]*Backend // keyed by lowercase name
	running  bool
}

// StoragePersist is the persistence interface the Manager needs. The concrete
// implementation is *storage.Store.
type StoragePersist interface {
	Storage
	GetBackends() ([]*model.Backend, error)
	AddBackend(b *model.Backend) (int64, error)
	DeleteBackend(id int64) error
	UpdateBackendUDP(id int64, port int, addr string) error
	ClearBackendUDP(id int64) error
}

// SetNotifier wires the asynchronous alert sink. It must be called before
// Start/Add so runtime backends capture the final notifier.
func (m *Manager) SetNotifier(n Notifier) { m.notify = n }

// NewManager wires a Manager. It must be Start()ed before use.
func NewManager(cfg *config.Config, log *logging.Logger, ms *metrics.Metrics,
	store StoragePersist, notify Notifier) *Manager {
	return &Manager{
		cfg:      cfg,
		log:      log,
		ms:       ms,
		store:    store,
		notify:   notify,
		tracker:  newIPConnTracker(),
		backends: make(map[string]*Backend),
	}
}

// Start loads every persisted backend and brings its listeners up.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	list, err := m.store.GetBackends()
	if err != nil {
		return fmt.Errorf("load backends: %w", err)
	}
	for _, b := range list {
		if !b.Enabled {
			continue
		}
		rt := newBackend(b, m.cfg, m.log, m.ms, m.store, m.notify, m.tracker)
		if err := rt.start(); err != nil {
			m.log.Error("backend startup failed", "backend", b.Name, "err", err)
			continue
		}
		m.backends[strings.ToLower(b.Name)] = rt
	}
	m.running = true
	m.log.Info("manager started", "backends", len(m.backends))
	return nil
}

// Shutdown gracefully drains and stops every backend. It returns when all
// accept loops have stopped (active connections are force-closed after grace).
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running = false
	list := make([]*Backend, 0, len(m.backends))
	for _, b := range m.backends {
		list = append(list, b)
	}
	var wg sync.WaitGroup
	for _, b := range list {
		wg.Add(1)
		go func(b *Backend) {
			defer wg.Done()
			b.shutdown(30 * time.Second)
		}(b)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		for _, b := range list {
			b.forceCloseConns()
		}
		return ctx.Err()
	}
	return nil
}

// List returns backends sorted by id.
func (m *Manager) List() []*Backend {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Backend, 0, len(m.backends))
	for _, b := range m.backends {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model().ID < out[j].Model().ID })
	return out
}

// Get returns a backend by name.
func (m *Manager) Get(name string) (*Backend, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.backends[strings.ToLower(name)]
	return b, ok
}

// Add creates, persists and starts a backend. If config.udp is non-empty a
// UDP listener is also opened.
func (m *Manager) Add(name string, listenPort int, backendTCP, backendUDP string, udpPort int) (*Backend, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name is required")
	}
	if listenPort <= 0 || listenPort > 65535 {
		return nil, errors.New("invalid listen_port")
	}
	if !m.cfg.Security.BackendAllowed(backendTCP) {
		return nil, fmt.Errorf("backend %q not in security.backend_whitelist", backendTCP)
	}
	host, _, err := net.SplitHostPort(backendTCP)
	if err != nil {
		return nil, fmt.Errorf("backend_tcp must be host:port: %w", err)
	}
	if host == "" {
		return nil, errors.New("backend_tcp host is empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.existsLocked(name) {
		return nil, fmt.Errorf("backend %q already exists", name)
	}
	if err := m.portFreeLocked(listenPort); err != nil {
		return nil, err
	}

	rec := &model.Backend{
		Name:       name,
		ListenPort: listenPort,
		BackendTCP: backendTCP,
		Enabled:    true,
	}
	if backendUDP != "" {
		if err := validateUDPAddr(backendUDP); err != nil {
			return nil, err
		}
		if udpPort <= 0 || udpPort > 65535 {
			return nil, errors.New("invalid udp port")
		}
		rec.UDPEnabled = true
		rec.UDPPort = udpPort
		rec.BackendUDP = backendUDP
	}
	id, err := m.store.AddBackend(rec)
	if err != nil {
		return nil, fmt.Errorf("persist backend: %w", err)
	}
	rec.ID = id

	rt := newBackend(rec, m.cfg, m.log, m.ms, m.store, m.notify, m.tracker)
	if err := rt.start(); err != nil {
		_ = m.store.DeleteBackend(id)
		return nil, fmt.Errorf("start backend: %w", err)
	}
	m.backends[strings.ToLower(name)] = rt
	m.log.Info("backend added", "name", name, "id", id)
	return rt, nil
}

// Remove gracefully drains a backend and deletes it by name or id.
func (m *Manager) Remove(ref string) error {
	key := strings.ToLower(strings.TrimSpace(ref))
	m.mu.Lock()
	rt, ok := m.backends[key]
	if !ok {
		// try by numeric id
		var id int64
		if _, err := fmt.Sscanf(ref, "%d", &id); err == nil {
			for _, b := range m.backends {
				if b.Model().ID == id {
					rt, ok = b, true
					break
				}
			}
		}
	}
	if !ok {
		return ErrNotFound
	}
	delete(m.backends, key)
	m.mu.Unlock()

	rt.shutdown(30 * time.Second)
	if err := m.store.DeleteBackend(rt.Model().ID); err != nil {
		return fmt.Errorf("delete backend: %w", err)
	}
	m.log.Info("backend removed", "name", rt.Name())
	return nil
}

// Restart re-creates the listeners for a backend by name.
func (m *Manager) Restart(name string) error {
	rt, ok := m.Get(name)
	if !ok {
		return ErrNotFound
	}
	if err := rt.restart(false); err != nil {
		return fmt.Errorf("restart %s: %w", name, err)
	}
	m.log.Info("backend restarted", "name", name)
	return nil
}

// AddUDP attaches a UDP listener to an existing backend.
func (m *Manager) AddUDP(name, backendUDP string, udpPort int) error {
	if err := validateUDPAddr(backendUDP); err != nil {
		return err
	}
	if udpPort <= 0 || udpPort > 65535 {
		return errors.New("invalid udp port")
	}
	rt, ok := m.Get(name)
	if !ok {
		return ErrNotFound
	}
	rec := rt.Model()
	if rec.UDPEnabled {
		return errors.New("udp already enabled")
	}
	m.mu.Lock()
	if err := m.portFreeLocked(udpPort); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	if err := m.store.UpdateBackendUDP(rec.ID, udpPort, backendUDP); err != nil {
		return err
	}
	// refresh the runtime model and start the udp forwarder
	rt.model.UDPEnabled = true
	rt.model.UDPPort = udpPort
	rt.model.BackendUDP = backendUDP
	return rt.startUDP()
}

// RemoveUDP detaches the UDP listener of a backend.
func (m *Manager) RemoveUDP(name string) error {
	rt, ok := m.Get(name)
	if !ok {
		return ErrNotFound
	}
	rec := rt.Model()
	if !rec.UDPEnabled {
		return errors.New("udp not enabled")
	}
	if rt.udpFwd != nil {
		rt.udpFwd.Close()
		rt.udpFwd = nil
	}
	rt.model.UDPEnabled = false
	rt.model.UDPPort = 0
	rt.model.BackendUDP = ""
	return m.store.ClearBackendUDP(rec.ID)
}

func (m *Manager) existsLocked(name string) bool {
	_, ok := m.backends[strings.ToLower(name)]
	return ok
}

// portFreeLocked verifies the port is not already bound by another backend.
func (m *Manager) portFreeLocked(port int) error {
	for _, b := range m.backends {
		if b.Model().ListenPort == port {
			return fmt.Errorf("port %d already used by backend %q", port, b.Name())
		}
		if b.Model().UDPEnabled && b.Model().UDPPort == port {
			return fmt.Errorf("udp port %d already used by backend %q", port, b.Name())
		}
	}
	return nil
}

func validateUDPAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("backend_udp must be host:port: %w", err)
	}
	if host == "" || port == "" {
		return errors.New("backend_udp host/port empty")
	}
	return nil
}
