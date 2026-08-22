// Package metrics holds lightweight in-memory counters used for stats and
// DDoS detection. The hot paths use atomic primitives; the per-IP rate
// tracker shards buckets by second and is pruned by a background janitor.
package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// Metrics aggregates global and per-IP counters.
type Metrics struct {
	ConnectionsTotal atomic.Int64
	UDPPacketsTotal  atomic.Int64
	ErrorsTotal      atomic.Int64

	ErrDial       atomic.Int64
	ErrBackendUp  atomic.Int64
	ErrRead       atomic.Int64
	ErrWrite      atomic.Int64
	ErrTimeout    atomic.Int64
	ErrRateLimit  atomic.Int64
	ErrUDPOversize atomic.Int64

	perIP *perIPTracker
}

// New constructs a Metrics and starts the janitor.
func New() *Metrics {
	m := &Metrics{perIP: newTracker()}
	m.perIP.start()
	return m
}

// IncConnection records a new accepted connection for the ip.
func (m *Metrics) IncConnection(ip string) {
	m.ConnectionsTotal.Add(1)
	m.perIP.inc(ip, connKind)
}

// IncUDPPacket records a UDP packet for the ip.
func (m *Metrics) IncUDPPacket(ip string) {
	m.UDPPacketsTotal.Add(1)
	m.perIP.inc(ip, udpKind)
}

// ConnectionsPerMin returns the number of connections opened by ip in the
// last 60 seconds.
func (m *Metrics) ConnectionsPerMin(ip string) int64 {
	return m.perIP.sum(ip, connKind, 60)
}

// UDPPacketsPerMin returns the number of UDP packets sent by ip in the last
// 60 seconds.
func (m *Metrics) UDPPacketsPerMin(ip string) int64 {
	return m.perIP.sum(ip, udpKind, 60)
}

// UDPPacketsPerSec returns the number of UDP packets sent by ip in the
// current one-second bucket.
func (m *Metrics) UDPPacketsPerSec(ip string) int64 {
	return m.perIP.sum(ip, udpKind, 1)
}

// Stop halts the janitor goroutine.
func (m *Metrics) Stop() { m.perIP.stop() }

type perKind int

const (
	connKind perKind = iota
	udpKind
)

// bucketSlots keeps one prefilled number of seconds for each kind.
const kindCount = 2

type ipBucket struct {
	strings map[int64]*[kindCount]*atomic.Int64
}

type perIPTracker struct {
	mu    sync.Mutex
	ips   map[string]*ipBucket
	stopCh chan struct{}
}

func newTracker() *perIPTracker {
	return &perIPTracker{ips: make(map[string]*ipBucket), stopCh: make(chan struct{})}
}

func (t *perIPTracker) start() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		cutoff := time.Now().Add(-90 * time.Second).Unix()
		for {
			select {
			case <-t.stopCh:
				return
			case <-ticker.C:
				cutoff = time.Now().Add(-90 * time.Second).Unix()
				t.prune(cutoff)
			}
		}
	}()
}

func (t *perIPTracker) stop() { close(t.stopCh) }

func (t *perIPTracker) inc(ip string, kind perKind) {
	second := time.Now().Unix()
	t.mu.Lock()
	b, ok := t.ips[ip]
	if !ok {
		b = &ipBucket{strings: make(map[int64]*[kindCount]*atomic.Int64)}
		t.ips[ip] = b
	}
	t.mu.Unlock()

	slots, ok := b.strings[second]
	if !ok {
		t.mu.Lock()
		slots, ok = b.strings[second]
		if !ok {
			slots = &[kindCount]*atomic.Int64{&atomic.Int64{}, &atomic.Int64{}}
			b.strings[second] = slots
		}
		t.mu.Unlock()
	}
	slots[kind].Add(1)
}

func (t *perIPTracker) sum(ip string, kind perKind, seconds int) int64 {
	now := time.Now().Unix()
	t.mu.Lock()
	defer t.mu.Unlock()
	b, ok := t.ips[ip]
	if !ok {
		return 0
	}
	var total int64
	for s, slots := range b.strings {
		if now-s <= int64(seconds) {
			total += slots[kind].Load()
		}
	}
	return total
}

func (t *perIPTracker) prune(cutoff int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for ip, b := range t.ips {
		for s := range b.strings {
			if s < cutoff {
				delete(b.strings, s)
			}
		}
		if len(b.strings) == 0 {
			delete(t.ips, ip)
		}
	}
}
