// Command proxygo runs the hybrid TCP/UDP proxy with a Telegram admin bot and
// SQLite-persisted state. Without a subcommand it runs the daemon; admin data
// subcommands (backends/bans/stats) read the database and exit.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"
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
	var showVersion bool
	flag.StringVar(&configPath, "config", "config.yaml", "path to YAML config")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Printf("proxygo %s\n", version)
		return
	}

	if args := flag.Args(); len(args) > 0 {
		if err := runAdmin(configPath, args); err != nil {
			fmt.Fprintln(os.Stderr, "proxygo:", err)
			os.Exit(1)
		}
		return
	}

	if err := serve(configPath); err != nil {
		fmt.Fprintln(os.Stderr, "proxygo:", err)
		os.Exit(1)
	}
}

// serve runs the proxy daemon.
func serve(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	log, err := logging.New(cfg.Logging)
	if err != nil {
		return err
	}
	defer log.Close()
	log.Info("starting proxygo", "version", version, "config", cfg.String())

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
		notifier.Notify("🔄 proxygo остановлен (graceful).")
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

// ---------- admin data subcommands -----------------------------------------

func runAdmin(configPath string, args []string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	store, err := storage.New(cfg.Storage.Path)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer store.Close()

	switch args[0] {
	case "backends":
		return adminBackends(store)
	case "bans":
		return adminBans(store)
	case "stats":
		return adminStats(store)
	default:
		return fmt.Errorf("unknown command %q (want: backends, bans, stats, run, -version)", args[0])
	}
}

func adminBackends(s *storage.Store) error {
	list, err := s.GetBackends()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tTCP \u2192\tUDP \u2192\tENABLED\tCREATED")
	for _, b := range list {
		udp := "—"
		if b.UDPEnabled {
			udp = fmt.Sprintf("%d \u2192 %s", b.UDPPort, b.BackendUDP)
		}
		fmt.Fprintf(w, "%d\t%s\t%d \u2192 %s\t%s\t%v\t%s\n",
			b.ID, b.Name, b.ListenPort, b.BackendTCP, udp,
			b.Enabled, time.Unix(b.CreatedAt, 0).Format("2006-01-02 15:04"))
	}
	return w.Flush()
}

func adminBans(s *storage.Store) error {
	list, err := s.Bans()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "IP\tREASON\tBY\tWHEN")
	for _, bn := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", bn.IP, bn.Reason, bn.CreatedBy,
			time.Unix(bn.CreatedAt, 0).Format("2006-01-02 15:04"))
	}
	return w.Flush()
}

func adminStats(s *storage.Store) error {
	backends, err := s.GetBackends()
	if err != nil {
		return err
	}
	sum, err := s.SumStats()
	if err != nil {
		return err
	}
	type row struct {
		name     string
		tcpBytes int64
		udpBytes int64
		conns    int64
		pkts     int64
	}
	var rows []row
	var totTCP, totUDP int64
	for _, b := range backends {
		v := sum[b.ID]
		r := row{name: b.Name, tcpBytes: v[0], udpBytes: v[1], conns: v[2], pkts: v[3]}
		rows = append(rows, r)
		totTCP += v[0]
		totUDP += v[1]
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].tcpBytes+rows[i].udpBytes > rows[j].tcpBytes+rows[j].udpBytes })

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "BACKEND\tTCP BYTES\tUDP BYTES\tTCP CONNS\tUDP PKTS")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\n", r.name, r.tcpBytes, r.udpBytes, r.conns, r.pkts)
	}
	fmt.Fprintf(w, "TOTAL\t%d\t%d\t\t\n", totTCP, totUDP)
	return w.Flush()
}
