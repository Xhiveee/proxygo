// Package logging provides structured (JSON) logging plus per-backend
// access logs. High-signal events (DDoS, backend down, restart) can be
// emitted to a subscriber (the Telegram notifier).
package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"proxygo/internal/config"
)

// Event is a high-signal notification forwarded to the bot asynchronously.
type Event struct {
	Text string
}

// Logger is the application logger. Safe for concurrent use.
type Logger struct {
	base     *slog.Logger
	level    slog.Level
	accessDir string

	mu       sync.Mutex
	access   map[string]*os.File
	notifier chan Event
	closed   bool
}

// New builds a Logger from the logging config.
func New(cfg config.Logging) (*Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	var writer io.Writer = os.Stderr
	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(cfg.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		writer = io.MultiWriter(os.Stderr, f)
	}

	var h slog.Handler
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(cfg.Format) {
	case "console", "text":
		h = slog.NewTextHandler(writer, opts)
	default: // json
		h = slog.NewJSONHandler(writer, opts)
	}

	if cfg.AccessLogDir != "" {
		if err := os.MkdirAll(cfg.AccessLogDir, 0o755); err != nil {
			return nil, err
		}
	}

	return &Logger{
		base:      slog.New(h),
		level:     level,
		accessDir: cfg.AccessLogDir,
		access:    make(map[string]*os.File),
		notifier:  make(chan Event, 256),
	}, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug", "":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, nil
	}
}

// Slog exposes the underlying structured logger for generic use.
func (l *Logger) Slog() *slog.Logger { return l.base }

// Access returns a logger that writes event lines to the given backend's
// access log. Protocol is "tcp" or "udp".
func (l *Logger) Access(backend, protocol string) *slog.Logger {
	name := backend + ":" + protocol
	file := l.accessFile(name)
	return slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func (l *Logger) accessFile(name string) io.Writer {
	l.mu.Lock()
	defer l.mu.Unlock()
	if f, ok := l.access[name]; ok {
		return f
	}
	if l.accessDir == "" {
		return io.Discard
	}
	f, err := os.OpenFile(filepath.Join(l.accessDir, name+".json.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		l.base.Error("access log open failed", slog.String("file", name), slog.Any("err", err))
		return io.Discard
	}
	l.access[name] = f
	return f
}

// Notifier returns the channel that high-signal events are streamed to.
func (l *Logger) Notifier() <-chan Event { return l.notifier }

// Notify non-blockingly queues an event for the Telegram notifier. If the
// buffer is full the event is dropped to never block the proxy hot path.
func (l *Logger) Notify(text string) {
	select {
	case l.notifier <- Event{Text: text}:
	default:
		l.base.Warn("notifier buffer full; dropping event")
	}
}

// Close flushes and closes open files and the notifier channel.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	for _, f := range l.access {
		_ = f.Close()
	}
	return nil
}

// helpers to make common log calls terser ------------------------------------

// Info logs an info line with optional key/values.
func (l *Logger) Info(msg string, args ...any) { l.base.Info(msg, args...) }

// Warn logs a warning line.
func (l *Logger) Warn(msg string, args ...any) { l.base.Warn(msg, args...) }

// Error logs an error line.
func (l *Logger) Error(msg string, args ...any) { l.base.Error(msg, args...) }

// Debug logs a debug line.
func (l *Logger) Debug(msg string, args ...any) { l.base.Debug(msg, args...) }
