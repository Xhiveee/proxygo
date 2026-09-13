-- 002_forward_mode.sql - per-backend forwarding mode for TCP.
-- raw    : transparent pipe (default)
-- bungee : rewrite the MC handshake with BungeeCord IP forwarding
-- ppv2   : prepend a PROXY v2 header (Paper/Velocity native support)
ALTER TABLE backends ADD COLUMN forward_mode TEXT NOT NULL DEFAULT 'raw';
