// Package model defines the shared data types persisted to SQLite and
// exchanged between the proxy, storage and admin layers.
package model

import "time"

// Backend is a single proxied destination. TCP always has a listener; UDP is
// optional per backend.
type Backend struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	ListenPort int   `json:"listen_port"`      // TCP listen port
	BackendTCP string `json:"backend_tcp"`     // host:port
	UDPEnabled bool  `json:"udp_enabled"`
	UDPPort   int    `json:"udp_port"`         // UDP listen port (0 = disabled)
	BackendUDP string `json:"backend_udp"`     // host:port
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// Ban is a banned client IP.
type Ban struct {
	IP        string `json:"ip"`
	Reason    string `json:"reason"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
}

// StatPoint is a single aggregation sample written to stats_hourly.
type StatPoint struct {
	BackendID  int64
	Hour       int64 // unix hour
	TCPBytes   int64
	UDPBytes   int64
	TCPConns   int64
	UDPPackets int64
}

// BackendStats is a read model combining a backend with aggregated traffic.
type BackendStats struct {
	Backend
	Uptime        time.Duration `json:"-"`
	ActiveConns   int           `json:"active_conns"`
	TCPBytes      int64         `json:"tcp_bytes"`
	UDPBytes      int64         `json:"udp_bytes"`
	TCPConns      int64         `json:"tcp_conns"`
	UDPPackets    int64         `json:"udp_packets"`
	TCPProxyConns int64         `json:"tcp_proxy_conns"`
}

// AuditEntry is one line of the admin audit trail.
type AuditEntry struct {
	ID        int64  `json:"id"`
	ActorID   int64  `json:"actor_id"`
	Action    string `json:"action"`
	Detail    string `json:"detail"`
	CreatedAt int64  `json:"created_at"`
}
