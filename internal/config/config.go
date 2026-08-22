package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root application configuration, loaded from a YAML file.
type Config struct {
	Telegram Telegram `yaml:"telegram"`
	Proxy    Proxy    `yaml:"proxy"`
	Storage  Storage  `yaml:"storage"`
	Logging  Logging  `yaml:"logging"`
	Security Security `yaml:"security"`
}

// Telegram configures the admin bot.
type Telegram struct {
	BotToken          string  `yaml:"bot_token"`
	AdminIDs          []int64 `yaml:"admin_ids"`
	RateLimitPerMin   int     `yaml:"rate_limit_per_minute"`
	Disabled          bool    `yaml:"disabled"`
	PollTimeoutSecond int     `yaml:"poll_timeout_seconds"`
	// Proxy is an optional HTTP/SOCKS proxy (e.g. socks5://127.0.0.1:1080 or
	// http://proxy:8080) used to reach api.telegram.org when it is blocked on
	// the local network. Empty = direct connection.
	Proxy string `yaml:"proxy"`
}

// Proxy holds runtime tuning for the forwarding layer.
type Proxy struct {
	DefaultIdleTimeout string `yaml:"default_idle_timeout"`
	UDPSessionTimeout  string `yaml:"udp_session_timeout"`
	BufferSize         int    `yaml:"buffer_size"`
	ListenInterface    string `yaml:"listen_interface"`

	// parsed runtime values
	DefaultIdle time.Duration `yaml:"-"`
	UDPSession  time.Duration `yaml:"-"`
}

// Storage configures the SQLite database.
type Storage struct {
	Path string `yaml:"path"`
}

// Logging configures log output.
type Logging struct {
	Level         string `yaml:"level"`
	File          string `yaml:"file"`
	AccessLogDir  string `yaml:"access_log_dir"`
	Format        string `yaml:"format"` // json | console
}

// Security holds ban and rate-limit settings.
type Security struct {
	BackendWhitelist      []string `yaml:"backend_whitelist"`
	MaxTCPConnsPerIP      int      `yaml:"max_tcp_connections_per_ip"`
	MaxUDPPacketsPerIPSec int      `yaml:"max_udp_packets_per_ip_per_sec"`
	MaxTCPConnsPerMin     int `yaml:"max_tcp_connections_per_min_ddos"`
	MaxUDPPacketsPerMin   int `yaml:"max_udp_packets_per_min_ddos"`
	EnabledIptables       bool `yaml:"enforce_iptables"`

	CachedBackendWhitelist map[string]struct{} `yaml:"-"`
}

// Default returns a Config populated with sane defaults.
func Default() *Config {
	return &Config{
		Telegram: Telegram{
			RateLimitPerMin:   10,
			PollTimeoutSecond: 30,
		},
		Proxy: Proxy{
			DefaultIdleTimeout: "30m",
			UDPSessionTimeout:  "60s",
			BufferSize:         32768,
			ListenInterface:    "0.0.0.0",
		},
		Storage: Storage{Path: "/opt/proxygo/data/proxygo.db"},
		Logging: Logging{Level: "info", Format: "json", AccessLogDir: "/opt/proxygo/log/access/"},
		Security: Security{
			MaxTCPConnsPerIP:      3,
			MaxUDPPacketsPerIPSec: 100,
			MaxTCPConnsPerMin:     100,
			MaxUDPPacketsPerMin:   1000,
			EnabledIptables:       true,
		},
	}
}

// Load reads and parses the YAML config from path, applying defaults for
// any omitted fields, then validates and expands runtime values.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.expand(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) expand() error {
	var err error
	if c.Proxy.DefaultIdle, err = time.ParseDuration(c.Proxy.DefaultIdleTimeout); err != nil {
		return fmt.Errorf("proxy.default_idle_timeout: %w", err)
	}
	if c.Proxy.UDPSession, err = time.ParseDuration(c.Proxy.UDPSessionTimeout); err != nil {
		return fmt.Errorf("proxy.udp_session_timeout: %w", err)
	}
	if c.Proxy.BufferSize <= 0 {
		c.Proxy.BufferSize = 32768
	}
	if c.Proxy.ListenInterface == "" {
		c.Proxy.ListenInterface = "0.0.0.0"
	}
	c.Security.CachedBackendWhitelist = make(map[string]struct{}, len(c.Security.BackendWhitelist))
	for _, ip := range c.Security.BackendWhitelist {
		c.Security.CachedBackendWhitelist[ip] = struct{}{}
	}
	return nil
}

// Validate performs structural validation after defaults are applied.
func (c *Config) Validate() error {
	if c.Proxy.DefaultIdle <= 0 {
		return fmt.Errorf("valid idle_timeout required")
	}
	if c.Proxy.UDPSession <= 0 {
		return fmt.Errorf("valid udp_session_timeout required")
	}
	if c.Proxy.BufferSize < 1024 || c.Proxy.BufferSize > 1<<20 {
		return fmt.Errorf("buffer_size out of range [1024, 1MiB]")
	}
	if c.Storage.Path == "" {
		return fmt.Errorf("storage.path is required")
	}
	if c.Security.MaxTCPConnsPerIP <= 0 {
		return fmt.Errorf("security.max_tcp_connections_per_ip must be > 0")
	}
	if c.Security.MaxUDPPacketsPerIPSec <= 0 {
		return fmt.Errorf("security.max_udp_packets_per_ip_per_sec must be > 0")
	}
	if c.Telegram.RateLimitPerMin <= 0 {
		return fmt.Errorf("telegram.rate_limit_per_minute must be > 0")
	}
	return nil
}

// TeleThro throttling is derived from rate limit.
func (c *Config) TeleRateLimit() time.Duration {
	per := time.Duration(60/c.Telegram.RateLimitPerMin) * time.Second
	if per <= 0 {
		per = time.Second
	}
	return per
}

// BackendAllowed reports whether the ip:port is permitted by the backend
// whitelist. An empty whitelist allows everything.
func (s Security) BackendAllowed(hostport string) bool {
	if len(s.CachedBackendWhitelist) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if _, ok := s.CachedBackendWhitelist[host]; ok {
		return true
	}
	// allow CIDR entries as well
	for entry := range s.CachedBackendWhitelist {
		if _, ipnet, err := net.ParseCIDR(entry); err == nil {
			if ip := net.ParseIP(host); ip != nil && ipnet.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// AdminAllowed reports whether the telegram id is a permitted admin.
func (c *Config) AdminAllowed(id int64) bool {
	for _, a := range c.Telegram.AdminIDs {
		if a == id {
			return true
		}
	}
	return false
}

// String returns a redacted summary for logging.
func (c *Config) String() string {
	var b strings.Builder
	b.WriteString("telegram.admin_ids=")
	for i, a := range c.Telegram.AdminIDs {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(strconv.FormatInt(a, 10))
	}
	b.WriteString(fmt.Sprintf(" proxy.listen=%s buff=%d", c.Proxy.ListenInterface, c.Proxy.BufferSize))
	b.WriteString(" storage=" + c.Storage.Path)
	return b.String()
}
