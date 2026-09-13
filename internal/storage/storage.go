// Package storage persists backends, bans, hourly stats and the audit log to
// SQLite using the pure-Go modernc.org/sqlite driver (no CGO).
package storage

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"time"

	_ "modernc.org/sqlite"

	"proxygo/internal/model"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps the SQL database and exposes typed helpers.
type Store struct {
	db *sql.DB
}

// New opens (creating if needed) the database at path and runs migrations.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc sqlite is single-writer; a single conn avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("pragma: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// SQL returns the underlying *sql.DB (used by tests and introspection).
func (s *Store) SQL() *sql.DB { return s.db }

func (s *Store) migrate() error {
	dirs, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		path TEXT PRIMARY KEY, applied_at INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	names := make([]string, 0, len(dirs))
	for _, d := range dirs {
		names = append(names, d.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var applied int
		if err := s.db.QueryRow(
			`SELECT COUNT(1) FROM schema_migrations WHERE path=?`, name).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		body, rerr := migrationsFS.ReadFile("migrations/" + name)
		if rerr != nil {
			return rerr
		}
		if _, err := s.db.Exec(string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO schema_migrations(path, applied_at) VALUES(?, ?)`,
			name, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the underlying connection pool.
func (s *Store) Close() error { return s.db.Close() }

// ---------- backends -------------------------------------------------------

const backendCols = `id, name, listen_port, backend_tcp, forward_mode,
	udp_enabled, udp_port, backend_udp, enabled, created_at, updated_at`

// AddBackend inserts a new backend and returns its id.
func (s *Store) AddBackend(b *model.Backend) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO backends
		(name, listen_port, backend_tcp, forward_mode, udp_enabled, udp_port, backend_udp, enabled, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		b.Name, b.ListenPort, b.BackendTCP, b.ForwardMode, b.UDPEnabled, b.UDPPort, b.BackendUDP, b.Enabled,
		time.Now().Unix(), time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetBackends returns all backends, newest first.
func (s *Store) GetBackends() ([]*model.Backend, error) {
	rows, err := s.db.Query(`SELECT ` + backendCols + ` FROM backends ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Backend
	for rows.Next() {
		b, err := scanBackend(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) GetBackendByID(id int64) (*model.Backend, error) {
	row := s.db.QueryRow(`SELECT `+backendCols+` FROM backends WHERE id=?`, id)
	b, err := scanBackend(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return b, err
}

func (s *Store) GetBackendByName(name string) (*model.Backend, error) {
	row := s.db.QueryRow(`SELECT `+backendCols+` FROM backends WHERE name=?`, name)
	b, err := scanBackend(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return b, err
}

func scanBackend(sc interface{ Scan(...any) error }) (*model.Backend, error) {
	var b model.Backend
	err := sc.Scan(&b.ID, &b.Name, &b.ListenPort, &b.BackendTCP, &b.ForwardMode,
		&b.UDPEnabled, &b.UDPPort, &b.BackendUDP, &b.Enabled, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// UpdateBackendForwardMode sets the TCP forwarding mode of a backend.
func (s *Store) UpdateBackendForwardMode(id int64, mode string) error {
	_, err := s.db.Exec(`UPDATE backends SET forward_mode=?, updated_at=? WHERE id=?`,
		mode, time.Now().Unix(), id)
	return err
}

// UpdateBackendUDP attaches a UDP listener to a backend.
func (s *Store) UpdateBackendUDP(id int64, udpPort int, udpAddr string) error {
	_, err := s.db.Exec(`UPDATE backends SET udp_enabled=1, udp_port=?, backend_udp=?,
		updated_at=? WHERE id=?`, udpPort, udpAddr, time.Now().Unix(), id)
	return err
}

// ClearBackendUDP detaches the UDP listener from a backend.
func (s *Store) ClearBackendUDP(id int64) error {
	_, err := s.db.Exec(`UPDATE backends SET udp_enabled=0, udp_port=0, backend_udp='',
		updated_at=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

// DeleteBackend removes a backend row.
func (s *Store) DeleteBackend(id int64) error {
	_, err := s.db.Exec(`DELETE FROM backends WHERE id=?`, id)
	return err
}

// ---------- bans -----------------------------------------------------------

// BanIP records a ban for ip.
func (s *Store) BanIP(ip, reason, actor string) error {
	_, err := s.db.Exec(`INSERT INTO bans(ip, reason, created_by, created_at) VALUES(?,?,?,?)
		ON CONFLICT(ip) DO UPDATE SET reason=excluded.reason, created_by=excluded.created_by,
		created_at=excluded.created_at`,
		ip, reason, actor, time.Now().Unix())
	return err
}

// UnbanIP removes the ban for ip.
func (s *Store) UnbanIP(ip string) error {
	_, err := s.db.Exec(`DELETE FROM bans WHERE ip=?`, ip)
	return err
}

// Bans lists all active bans.
func (s *Store) Bans() ([]*model.Ban, error) {
	rows, err := s.db.Query(`SELECT ip, reason, created_by, created_at FROM bans ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Ban
	for rows.Next() {
		var b model.Ban
		if err := rows.Scan(&b.IP, &b.Reason, &b.CreatedBy, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// IsBanned reports whether ip has an active ban.
func (s *Store) IsBanned(ip string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM bans WHERE ip=?`, ip).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---------- hourly stats ---------------------------------------------------

// RecordStat upserts one hourly aggregation row.
func (s *Store) RecordStat(p model.StatPoint) error {
	_, err := s.db.Exec(`INSERT INTO stats_hourly
			(backend_id, hour_start, tcp_bytes, udp_bytes, tcp_conns, udp_packets)
			VALUES (?,?,?,?,?,?)
			ON CONFLICT(backend_id, hour_start) DO UPDATE SET
				tcp_bytes=tcp_bytes+excluded.tcp_bytes,
				udp_bytes=udp_bytes+excluded.udp_bytes,
				tcp_conns=tcp_conns+excluded.tcp_conns,
				udp_packets=udp_packets+excluded.udp_packets`,
		p.BackendID, p.Hour, p.TCPBytes, p.UDPBytes, p.TCPConns, p.UDPPackets)
	return err
}

// SumStats returns per-backend aggregates over all recorded hours.
func (s *Store) SumStats() (map[int64][4]int64, error) {
	rows, err := s.db.Query(`SELECT backend_id, SUM(tcp_bytes), SUM(udp_bytes),
		SUM(tcp_conns), SUM(udp_packets) FROM stats_hourly GROUP BY backend_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64][4]int64)
	for rows.Next() {
		var id int64
		var v [4]int64
		if err := rows.Scan(&id, &v[0], &v[1], &v[2], &v[3]); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// ---------- audit ----------------------------------------------------------

// RecordAudit appends an admin action to the audit trail.
func (s *Store) RecordAudit(actor int64, action, detail string) error {
	_, err := s.db.Exec(`INSERT INTO audit_log(actor_id, action, detail, created_at)
		VALUES (?,?,?,?)`, actor, action, detail, time.Now().Unix())
	return err
}

// AuditLog returns the most recent `limit` audit entries.
func (s *Store) AuditLog(limit int) ([]*model.AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, actor_id, action, detail, created_at
		FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.AuditEntry
	for rows.Next() {
		var e model.AuditEntry
		if err := rows.Scan(&e.ID, &e.ActorID, &e.Action, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
