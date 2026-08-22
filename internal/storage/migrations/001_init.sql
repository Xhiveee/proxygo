-- 001_init.sql - initial schema for the MC hybrid proxy.
-- SQLite schema for the bc-go proxy: backends, bans, hourly stats, audit log.
PRAGMA foreign_keys = ON;

-- A single proxied destination. TCP is mandatory, UDP optional.
CREATE TABLE IF NOT EXISTS backends (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    listen_port  INTEGER NOT NULL,
    backend_tcp  TEXT    NOT NULL,
    udp_enabled  INTEGER NOT NULL DEFAULT 0,
    udp_port     INTEGER NOT NULL DEFAULT 0,
    backend_udp  TEXT    NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

-- Banned client IPs (internal ACL layer).
CREATE TABLE IF NOT EXISTS bans (
    ip         TEXT    PRIMARY KEY,
    reason     TEXT    NOT NULL DEFAULT '',
    created_by TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

-- Hourly aggregated traffic per backend.
CREATE TABLE IF NOT EXISTS stats_hourly (
    backend_id  INTEGER NOT NULL REFERENCES backends(id) ON DELETE CASCADE,
    hour_start  INTEGER NOT NULL,
    tcp_bytes   INTEGER NOT NULL DEFAULT 0,
    udp_bytes   INTEGER NOT NULL DEFAULT 0,
    tcp_conns   INTEGER NOT NULL DEFAULT 0,
    udp_packets INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (backend_id, hour_start)
);

-- Immutable admin action trail.
CREATE TABLE IF NOT EXISTS audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_id   INTEGER NOT NULL,
    action     TEXT    NOT NULL,
    detail     TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

-- Indexes for the hot queries.
CREATE INDEX IF NOT EXISTS idx_bans_created ON bans(created_at);
CREATE INDEX IF NOT EXISTS idx_stats_hourly_backend ON stats_hourly(backend_id, hour_start);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at);
