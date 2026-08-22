// Command mc-proxy runs the hybrid TCP/UDP proxy with a Telegram admin bot
// and SQLite-persisted state.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"proxygo/internal/config"
	"proxygo/internal/logging"
	"proxygo/internal/metrics"
	"proxygo/internal/proxy"
	"proxygo/internal/security"
	"proxygo/internal/storage"
	"proxygo/internal/telegram"
)

var version = "dev"

// logSink is a notifier used when Telegram is disabled; it only logs.
type logSink struct{ log *logging.Logger }

func (s logSink) Notify(text string) { s.log.Warn("alert: " + text) }

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "config.yaml", "path to YAML config")
	flag.Parse()

	if err := run(configPath); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	log, err := logging.New(cfg.Logging)
	if err != nil {
		return err
	}
	defer log.Close()
	log.Info("starting mc-proxy", "version", version, "config", cfg.String())

	store, err := storage.New(cfg.Storage.Path)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer store.Close()

	ms := metrics.New()
	defer ms.Stop()

	bans := security.NewBans(store, log, cfg.Security.EnabledIptables)

	mgr := proxy.NewManager(cfg, log, ms, store, nil)

	var notifier proxy.Notifier = logSink{log: log}
	if !cfg.Telegram.Disabled && cfg.Telegram.BotToken != "" {
		bot := telegram.NewBot(cfg, log, mgr, bans, store)
		mgr.SetNotifier(bot)
		notifier = bot
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g, ctx := errgroup.WithContext(rootCtx)

	// Drain high-signal logging events into the notifier.
	g.Go(func() error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case ev := <-log.Notifier():
				notifier.Notify(ev.Text)
			}
		}
	})

	if err := mgr.Start(ctx); err != nil {
		return err
	}

	if bot, ok := notifier.(*telegram.Bot); ok {
		g.Go(func() error { return bot.Start(ctx) })
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	g.Go(func() error {
		sig := <-sigCh
		log.Info("signal received", "sig", sig.String())
		cancel()

		shutdownCtx, sc := context.WithTimeout(context.Background(), 35*time.Second)
		defer sc()
		if err := mgr.Shutdown(shutdownCtx); err != nil {
			log.Error("shutdown error", "err", err)
		}
		notifier.Notify("рџ”„ mc-proxy РѕСЃС‚Р°РЅРѕРІР»РµРЅ (graceful).")
		return ctx.Err()
	})

	err = g.Wait()
	if err != nil && err != context.Canceled {
		log.Error("service error", "err", err)
		return err
	}
	log.Info("service stopped")
	return nil
}
