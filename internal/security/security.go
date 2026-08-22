// Package security provides IP ban management and simple rate limiting.
//
// Bans live in SQLite (the durable source of truth) and are mirrored to
// iptables on Linux as a best-effort first-line defence. On systems where
// iptables is unavailable (e.g. development or containers without
// NET_ADMIN) the persistence layer still works, so bans are enforced by the
// proxy layer regardless.
package security

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"proxygo/internal/logging"
	"proxygo/internal/model"
	"proxygo/internal/storage"
)

// Bans manages banned client IPs, mirroring them to iptables when possible.
type Bans struct {
	store *storage.Store
	log   *logging.Logger

	// chain is the iptables chain name used for dropped MC traffic.
	chain string
	// enforce controls whether iptables commands are issued at all.
	enforce bool
}

// NewBans builds a ban manager. enforceIptables may be false to skip all
// iptables interaction (handy in dev or unprivileged containers).
func NewBans(store *storage.Store, log *logging.Logger, enforceIptables bool) *Bans {
	b := &Bans{
		store:   store,
		log:     log,
		chain:   "MC-PROXY-BAN",
		enforce: enforceIptables,
	}
	if b.enforce {
		b.ensureChain()
	}
	return b
}

// Ban records a ban and (best-effort) drops the IP in iptables.
func (b *Bans) Ban(ip, reason, actor string) error {
	if ip == "" {
		return fmt.Errorf("empty ip")
	}
	if err := b.store.BanIP(ip, reason, actor); err != nil {
		return err
	}
	if b.enforce {
		b.iptables("add", ip)
	}
	b.log.Info("ban added", "ip", ip, "reason", reason, "by", actor)
	return nil
}

// Unban removes a ban and the corresponding iptables rule.
func (b *Bans) Unban(ip string) error {
	if err := b.store.UnbanIP(ip); err != nil {
		return err
	}
	if b.enforce {
		b.iptables("del", ip)
	}
	b.log.Info("ban removed", "ip", ip)
	return nil
}

// List returns all active bans.
func (b *Bans) List() ([]*model.Ban, error) { return b.store.Bans() }

func (b *Bans) ensureChain() {
	args := []string{"-w", "-N", b.chain}
	if err := exec.Command("iptables", args...).Run(); err != nil {
		// chain likely already exists; ignore
	}
	// never miss newly spawned processes (best-effort)
	_ = exec.Command("iptables", "-w", "-I", "INPUT", "-j", b.chain).Run()
	_ = exec.Command("iptables", "-w", "-I", "OUTPUT", "-j", b.chain).Run()
}

func (b *Bans) iptables(op, ip string) {
	arg := "-D"
	if op == "add" {
		arg = "-A"
	}
	// Drop both directions for a banned player IP on all ports.
	if err := exec.Command("iptables", "-w", arg, b.chain, "-s", ip, "-j", "DROP").Run(); err != nil {
		b.log.Warn("iptables add failed", "ip", ip, "err", err)
	}
	if err := exec.Command("iptables", "-w", arg, b.chain, "-d", ip, "-j", "DROP").Run(); err != nil {
		b.log.Warn("iptables add failed (dst)", "ip", ip, "err", err)
	}
}

// Describe returns a human readable ban summary.
func Describe(bs []*model.Ban, max int) string {
	if len(bs) == 0 {
		return "Нет банней."
	}
	if max <= 0 || max > len(bs) {
		max = len(bs)
	}
	var sb strings.Builder
	for _, bn := range bs[:max] {
		sb.WriteString(fmt.Sprintf("· %s — %s (by %s, %s)\n", bn.IP,
			orEmpty(bn.Reason), bn.CreatedBy, time.Unix(bn.CreatedAt, 0).Format("2006-01-02 15:04")))
	}
	return sb.String()
}

func orEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// ---------- rate limiting --------------------------------------------------

// Limiter is a simple lossy token bucket keyed by an arbitrary identifier.
type Limiter struct {
	capacity int
	refill   time.Duration

	mu      sync.Mutex
	tokens  map[string]*bucket
}

type bucket struct {
	tokens   int
	lastFill time.Time
}

// NewLimiter creates a limiter that grants `capacity` uses per refill
// interval. Example: capacity=10, refill=60s => 10 commands/minute.
func NewLimiter(capacity int, refill time.Duration) *Limiter {
	if capacity <= 0 {
		capacity = 1
	}
	if refill <= 0 {
		refill = time.Minute
	}
	return &Limiter{capacity: capacity, refill: refill, tokens: make(map[string]*bucket)}
}

// Allow reports whether `key` may proceed right now.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.tokens[key]
	if !ok {
		b = &bucket{tokens: l.capacity, lastFill: now}
		l.tokens[key] = b
	}
	elapsed := now.Sub(b.lastFill)
	if elapsed >= l.refill {
		b.tokens = l.capacity
		b.lastFill = now
	}
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// Prune drops stale keys to bound memory. Call periodically.
func (l *Limiter) Prune(maxAge time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	for k, b := range l.tokens {
		if b.lastFill.Before(cutoff) {
			delete(l.tokens, k)
		}
	}
}
