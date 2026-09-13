// Package telegram implements the admin bot that drives the proxy. It is the
// only authenticated entry point for live reconfiguration, stats and bans.
package telegram

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"proxygo/internal/config"
	"proxygo/internal/logging"
	"proxygo/internal/model"
	"proxygo/internal/proxy"
	"proxygo/internal/security"
	"proxygo/internal/storage"
)

// Bot wires the long-polling Telegram API to the proxy Manager.
type Bot struct {
	api   *tgbotapi.BotAPI
	cfg   *config.Config
	log   *logging.Logger
	mgr   *proxy.Manager
	bans  *security.Bans
	store *storage.Store

	limiter *security.Limiter

	notifCh  chan string
	done     chan struct{}
	doneOnce sync.Once
}

// NewBot builds the bot. Call Start to begin polling.
func NewBot(cfg *config.Config, log *logging.Logger, mgr *proxy.Manager,
	bans *security.Bans, store *storage.Store) *Bot {
	return &Bot{
		cfg:     cfg,
		log:     log,
		mgr:     mgr,
		bans:    bans,
		store:   store,
		limiter: security.NewLimiter(cfg.Telegram.RateLimitPerMin, time.Minute),
		notifCh: make(chan string, 256),
		done:    make(chan struct{}),
	}
}

// Notify implements proxy.Notifier. It queues a message for delivery to all
// admins and never blocks the proxy hot path.
func (b *Bot) Notify(text string) {
	select {
	case b.notifCh <- text:
	default:
		b.log.Warn("telegram notify queue full; drop", "text", text)
	}
}

// Start begins long polling and the async notifier consumer.
func (b *Bot) Start(ctx context.Context) error {
	token := strings.TrimSpace(b.cfg.Telegram.BotToken)
	if token == "" {
		return fmt.Errorf("telegram disabled (empty token)")
	}
	var api *tgbotapi.BotAPI
	var err error
	if p := strings.TrimSpace(b.cfg.Telegram.Proxy); p != "" {
		b.log.Info("telegram via proxy", "proxy", p)
		proxyURL, perr := url.Parse(p)
		if perr != nil {
			return fmt.Errorf("telegram proxy parse: %w", perr)
		}
		client := &http.Client{
			// No overall timeout: Telegram getUpdates long-poll can block for the
			// whole poll timeout. Only cap the connect so an unreachable network
			// fails fast instead of hanging.
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				DialContext: (&net.Dialer{
					Timeout: 15 * time.Second, KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		}
		api, err = tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, client)
	} else {
		api, err = tgbotapi.NewBotAPI(token)
	}
	if err != nil {
		return fmt.Errorf("telegram login (проверь bot_token; если api.telegram.org недоступен — настрой telegram.proxy=socks5://user:pass@host:port или http://host:8080, предварительно проверив, что до прокси достукивается сеть): %w", err)
	}
	b.api = api
	api.Debug = false
	b.log.Info("telegram bot connected", "user", api.Self.UserName,
		"admins", b.cfg.Telegram.AdminIDs, "n_admins", len(b.cfg.Telegram.AdminIDs))
	if len(b.cfg.Telegram.AdminIDs) == 0 {
		b.log.Warn("telegram admin_ids is empty — bot will ignore everyone")
	}

	// If a webhook was ever set (e.g. by another tool), long polling would
	// fail with 409 Conflict. Best-effort clear it.
	if _, werr := api.Request(tgbotapi.DeleteWebhookConfig{}); werr != nil {
		b.log.Warn("webhook cleanup failed", "err", werr)
	}

	go b.notifyLoop()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = b.cfg.Telegram.PollTimeoutSecond
	updates := api.GetUpdatesChan(u)

	go func() {
		<-ctx.Done()
		api.StopReceivingUpdates()
		b.doneOnce.Do(func() { close(b.done) })
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case upd, ok := <-updates:
			if !ok {
				return nil
			}
			b.handleUpdate(upd)
		}
	}
}

func (b *Bot) notifyLoop() {
	defer b.doneOnce.Do(func() { close(b.done) })
	for {
		select {
		case <-b.done:
			return
		case text := <-b.notifCh:
			b.broadcast(text)
		}
	}
}

// notifyLoop and Start both close b.done; guard against double close.
func (b *Bot) broadcast(text string) {
	for _, id := range b.cfg.Telegram.AdminIDs {
		msg := tgbotapi.NewMessage(id, text)
		msg.ParseMode = "HTML"
		_, _ = b.api.Send(msg)
	}
}

func (b *Bot) handleUpdate(upd tgbotapi.Update) {
	if upd.CallbackQuery != nil {
		b.handleCallback(upd.CallbackQuery)
		return
	}
	if upd.Message == nil {
		return
	}
	msg := upd.Message
	uid := msg.From.ID
	if !b.cfg.AdminAllowed(uid) {
		b.log.Warn("non-admin message ignored", "from", uid,
			"name", msg.From.FirstName+" "+msg.From.LastName,
			"allowed", b.cfg.Telegram.AdminIDs, "text", msg.Text)
		b.reply(msg, "⛔ Доступ только для администраторов.")
		return
	}
	if !b.limiter.Allow(strconv.FormatInt(uid, 10)) {
		b.reply(msg, "⏳ <b>Rate limit</b>. Подожди немного и попробуй снова.")
		return
	}
	b.dispatch(msg)
}

func (b *Bot) dispatch(msg *tgbotapi.Message) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	cmd, args := parseCommand(text)

	switch cmd {
	case "/start", "/help":
		b.send(msg, helpText)
	case "/list", "/backends":
		b.cmdList(msg)
	case "/add":
		b.cmdAdd(msg, args)
	case "/remove":
		b.cmdRemove(msg, args)
	case "/restart":
		b.cmdRestart(msg, args)
	case "/add-udp":
		b.cmdAddUDP(msg, args)
	case "/remove-udp":
		b.cmdRemoveUDP(msg, args)
	case "/forward":
		b.cmdForward(msg, args)
	case "/stats":
		b.cmdStats(msg, args)
	case "/ban":
		b.cmdBan(msg, args)
	case "/unban":
		b.cmdUnban(msg, args)
	case "/bans":
		b.cmdBans(msg)
	case "/log":
		b.cmdLog(msg, args)
	default:
		b.send(msg, "Неизвестная команда. /start — список команд.")
	}
}

// ---------- command handlers ----------------------------------------------

func (b *Bot) cmdList(msg *tgbotapi.Message) {
	list := b.mgr.List()
	if len(list) == 0 {
		b.send(msg, "Бэкендов нет. ` /add <name> <port> <tcp> [udp] `")
		return
	}
	var sb strings.Builder
	sb.WriteString("📦 <b>Бэкенды</b>\n")
	sb.WriteString("<code>ID  Name     TCP:→backend              UDP       Conns  Up     Tx</code>\n")
	for _, bk := range list {
		m := bk.Model()
		udp := "—"
		if m.UDPEnabled {
			udp = fmt.Sprintf("%d→%s", m.UDPPort, m.BackendUDP)
		}
		mode := ""
		if m.ForwardMode != "" && m.ForwardMode != model.ForwardRaw {
			mode = "·" + m.ForwardMode
		}
		sb.WriteString(fmt.Sprintf(
			"<code>%-3d %-8s %-24s %-14s %-5d %-7s %-8s</code>\n",
			m.ID, m.Name, fmt.Sprintf("%d→%s%s", m.ListenPort, m.BackendTCP, mode), udp,
			bk.ActiveConns(), shortUptime(time.Since(bk.StartedAt())), humanBytes(bkStatsTx(bk)),
		))
	}
	b.send(msg, sb.String())
}

func (b *Bot) cmdAdd(msg *tgbotapi.Message, args []string) {
	if len(args) < 3 || len(args) > 4 {
		b.send(msg, "Формат: <code>/add &lt;name&gt; &lt;port&gt; &lt;tcp&gt; [udp]</code>")
		return
	}
	name := args[0]
	port, err := strconv.Atoi(args[1])
	if err != nil {
		b.send(msg, "Порт должен быть числом.")
		return
	}
	backendTCP := args[2]
	backendUDP := ""
	if len(args) == 4 {
		backendUDP = args[3]
	}
	udpPort := 0
	if backendUDP != "" {
		udpPort = findPortFrom(backendUDP)
	}
	bk, err := b.mgr.Add(name, port, backendTCP, backendUDP, udpPort)
	if err != nil {
		b.send(msg, "❌ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/add", name)
	b.send(msg, "✅ Бэкенд добавлен: <b>"+bk.Name()+"</b>\nTCP "+bk.Model().BackendTCP)
}

func (b *Bot) cmdRemove(msg *tgbotapi.Message, args []string) {
	if len(args) != 1 {
		b.send(msg, "Формат: <code>/remove &lt;name|id&gt;</code>")
		return
	}
	ref := args[0]
	// confirm via inline button
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚠️ Удалить", "del:"+ref),
			tgbotapi.NewInlineKeyboardButtonData("Отмена", "cancel"),
		),
	)
	m := tgbotapi.NewMessage(msg.Chat.ID, "Удалить бэкенд <b>"+ref+"</b>?")
	m.ParseMode = "HTML"
	m.ReplyMarkup = kb
	_, _ = b.api.Send(m)
}

func (b *Bot) cmdRestart(msg *tgbotapi.Message, args []string) {
	if len(args) != 1 {
		b.send(msg, "Формат: <code>/restart &lt;name&gt;</code>")
		return
	}
	if err := b.mgr.Restart(args[0]); err != nil {
		b.send(msg, "❌ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/restart", args[0])
	b.send(msg, "🔄 Бэкенд <b>"+args[0]+"</b> пересоздан.")
}

func (b *Bot) cmdAddUDP(msg *tgbotapi.Message, args []string) {
	if len(args) != 2 {
		b.send(msg, "Формат: <code>/add-udp &lt;name&gt; &lt;udp&gt;</code>")
		return
	}
	udpPort := findPortFrom(args[1])
	if err := b.mgr.AddUDP(args[0], args[1], udpPort); err != nil {
		b.send(msg, "❌ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/add-udp", args[0])
	b.send(msg, "✅ UDP подключен к <b>"+args[0]+"</b> ("+args[1]+")")
}

func (b *Bot) cmdRemoveUDP(msg *tgbotapi.Message, args []string) {
	if len(args) != 1 {
		b.send(msg, "Формат: <code>/remove-udp &lt;name&gt;</code>")
		return
	}
	if err := b.mgr.RemoveUDP(args[0]); err != nil {
		b.send(msg, "❌ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/remove-udp", args[0])
	b.send(msg, "✅ UDP отключен у <b>"+args[0]+"</b>")
}

func (b *Bot) cmdForward(msg *tgbotapi.Message, args []string) {
	if len(args) != 2 {
		b.send(msg, "Формат: <code>/forward &lt;name&gt; &lt;raw|bungee|ppv2&gt;</code>\n"+
			"bungee — реальный IP через handshake (Spigot/Paper: bungeecord=true)\n"+
			"ppv2 — PROXY v2 (Paper: proxies.proxy-protocol=true)")
		return
	}
	if err := b.mgr.SetForwardMode(args[0], args[1]); err != nil {
		b.send(msg, "❌ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/forward", args[0]+" "+args[1])
	b.send(msg, "✅ Режим форвардинга для <b>"+args[0]+"</b>: "+args[1])
}

func (b *Bot) cmdStats(msg *tgbotapi.Message, args []string) {
	if len(args) == 1 {
		bk, ok := b.mgr.Get(args[0])
		if !ok {
			b.send(msg, "Бэкенд не найден.")
			return
		}
		b.send(msg, b.formatBackendStats(bk))
		return
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Обновить", "refresh:all"),
		),
	)
	m := tgbotapi.NewMessage(msg.Chat.ID, b.formatGlobalStats())
	m.ParseMode = "HTML"
	m.ReplyMarkup = kb
	_, _ = b.api.Send(m)
}

func (b *Bot) cmdBan(msg *tgbotapi.Message, args []string) {
	if len(args) < 1 {
		b.send(msg, "Формат: <code>/ban &lt;ip&gt; [причина]</code>")
		return
	}
	ip := args[0]
	reason := strings.Join(args[1:], " ")
	if err := b.bans.Ban(ip, reason, strconv.FormatInt(msg.From.ID, 10)); err != nil {
		b.send(msg, "❌ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/ban", ip+" "+reason)
	b.send(msg, "🔨 IP <b>"+ip+"</b> забанен: "+reason)
}

func (b *Bot) cmdUnban(msg *tgbotapi.Message, args []string) {
	if len(args) != 1 {
		b.send(msg, "Формат: <code>/unban &lt;ip&gt;</code>")
		return
	}
	if err := b.bans.Unban(args[0]); err != nil {
		b.send(msg, "❌ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/unban", args[0])
	b.send(msg, "✅ IP <b>"+args[0]+"</b> разбанен.")
}

func (b *Bot) cmdBans(msg *tgbotapi.Message) {
	list, err := b.bans.List()
	if err != nil {
		b.send(msg, "❌ "+err.Error())
		return
	}
	b.send(msg, "🔒 <b>Баны</b>\n"+security.Describe(list, 30))
}

func (b *Bot) cmdLog(msg *tgbotapi.Message, args []string) {
	n := 30
	if len(args) == 1 {
		if v, err := strconv.Atoi(args[0]); err == nil {
			n = v
		}
	}
	if cfg := b.cfg.Logging; cfg.File != "" {
		lines := tailFile(cfg.File, n)
		b.send(msg, "<pre>"+escapeHtml(lines)+"</pre>")
	} else {
		b.send(msg, "Лог файл не настроен.")
	}
}

// ---------- stats formatting ----------------------------------------------

func (b *Bot) formatGlobalStats() string {
	var sb strings.Builder
	sb.WriteString("📈 <b>Общая статистика</b>\n")
	totalTCP, totalUDP := int64(0), int64(0)
	for _, bk := range b.mgr.List() {
		in, out, udp, _, _ := bk.Stats()
		totalTCP += in + out
		totalUDP += udp
	}
	sb.WriteString(fmt.Sprintf("• <b>TCP</b> всего: %s\n", humanBytes(totalTCP)))
	sb.WriteString(fmt.Sprintf("• <b>UDP</b> всего: %s\n", humanBytes(totalUDP)))
	sb.WriteString("\n🏆 <b>Топ-5 бэкендов по трафику</b>\n")
	type row struct {
		name string
		v    int64
	}
	rows := make([]row, 0, len(b.mgr.List()))
	for _, bk := range b.mgr.List() {
		in, out, udp, _, _ := bk.Stats()
		rows = append(rows, row{bk.Name(), in + out + udp})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].v > rows[j].v })
	for i := 0; i < len(rows) && i < 5; i++ {
		sb.WriteString(fmt.Sprintf("%d. <b>%s</b> — %s\n", i+1, rows[i].name, humanBytes(rows[i].v)))
	}
	return sb.String()
}

func (b *Bot) formatBackendStats(bk *proxy.Backend) string {
	m := bk.Model()
	in, out, udp, pkts, conns := bk.Stats()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📊 <b>%s</b> (#%d)\n", m.Name, m.ID))
	sb.WriteString(fmt.Sprintf("TCP: <code>%d → %s</code> (%s, mode=%s)\n", m.ListenPort, m.BackendTCP, enabledStr(m.Enabled), m.ForwardMode))
	udpLine := "—"
	if m.UDPEnabled {
		udpLine = fmt.Sprintf("<code>%d → %s</code>", m.UDPPort, m.BackendUDP)
	}
	sb.WriteString("UDP: " + udpLine + "\n")
	sb.WriteString(fmt.Sprintf("Активных TCP: %d\n", bk.ActiveConns()))
	sb.WriteString(fmt.Sprintf("Соединений всего: %d\n", conns))
	sb.WriteString(fmt.Sprintf("TCP ↑ %s / ↓ %s\n", humanBytes(out), humanBytes(in)))
	sb.WriteString(fmt.Sprintf("UDP ↑ %s (%d пакетов)\n", humanBytes(udp), pkts))
	sb.WriteString(fmt.Sprintf("Аптайм: %s", shortUptime(time.Since(bk.StartedAt()))))
	return sb.String()
}

// ---------- callback -------------------------------------------------------

func (b *Bot) handleCallback(q *tgbotapi.CallbackQuery) {
	uid := q.From.ID
	if !b.cfg.AdminAllowed(uid) {
		b.answer(q, "только для админов")
		return
	}
	if !b.limiter.Allow(strconv.FormatInt(uid, 10)) {
		b.answer(q, "rate limit")
		return
	}
	data := q.Data
	chatID := q.Message.Chat.ID
	switch {
	case data == "cancel":
		b.answer(q, "отменено")
	case strings.HasPrefix(data, "del:"):
		ref := strings.TrimPrefix(data, "del:")
		if err := b.mgr.Remove(ref); err != nil {
			b.answer(q, "ошибка: "+err.Error())
			return
		}
		_ = b.store.RecordAudit(uid, "/remove", ref)
		b.editText(chatID, q.Message.MessageID, "🗑 Бэкенд <b>"+ref+"</b> удалён.")
		b.answer(q, "удалено")
	case strings.HasPrefix(data, "refresh:"):
		target := strings.TrimPrefix(data, "refresh:")
		if target == "all" {
			b.editText(chatID, q.Message.MessageID, b.formatGlobalStats())
		} else if bk, ok := b.mgr.Get(target); ok {
			b.editText(chatID, q.Message.MessageID, b.formatBackendStats(bk))
		}
		b.answer(q, "")
	default:
		b.answer(q, "")
	}
}

// ---------- message helpers ------------------------------------------------

func (b *Bot) send(msg *tgbotapi.Message, text string) {
	m := tgbotapi.NewMessage(msg.Chat.ID, text)
	m.ParseMode = "HTML"
	_, _ = b.api.Send(m)
}

func (b *Bot) reply(msg *tgbotapi.Message, text string) {
	m := tgbotapi.NewMessage(msg.Chat.ID, text)
	m.ParseMode = "HTML"
	_, _ = b.api.Send(m)
}

func (b *Bot) editText(chatID int64, msgID int, text string) {
	m := tgbotapi.NewEditMessageText(chatID, msgID, text)
	m.ParseMode = "HTML"
	_, _ = b.api.Send(m)
}

func (b *Bot) answer(q *tgbotapi.CallbackQuery, text string) {
	cb := tgbotapi.NewCallback(q.ID, text)
	_, _ = b.api.Request(cb)
}

const helpText = `<b>MC Hybrid Proxy Bot</b>

<b>Управление</b>
/list — таблица бэкендов
/add <b>name port tcp</b> [udp] — добавить
/add-udp <b>name udp</b> — добавить UDP
/remove-udp <b>name</b> — отключить UDP
/restart <b>name</b> — пересоздать listener
/remove <b>name|id</b> — удалить
/forward <b>name mode</b> — режим TCP: raw | bungee | ppv2 (реальный IP)

<b>Статистика</b>
/stats — общая
/stats <b>name</b> — по бэкенду

<b>Безопасность</b>
/ban <b>ip</b> [reason]
/unban <b>ip</b>
/bans

<b>Прочее</b>
/log <b>N</b> — последние N строк лога`
