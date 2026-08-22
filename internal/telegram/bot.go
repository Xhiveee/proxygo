// Package telegram implements the admin bot that drives the proxy. It is the
// only authenticated entry point for live reconfiguration, stats and bans.
package telegram

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"proxygo/internal/config"
	"proxygo/internal/logging"
	"proxygo/internal/proxy"
	"proxygo/internal/security"
	"proxygo/internal/storage"
)

// Bot wires the long-polling Telegram API to the proxy Manager.
type Bot struct {
	api  *tgbotapi.BotAPI
	cfg  *config.Config
	log  *logging.Logger
	mgr  *proxy.Manager
	bans *security.Bans
	store *storage.Store

	limiter *security.Limiter

	notifCh chan string
	done    chan struct{}
}

// NewBot builds the bot. Call Start to begin polling.
func NewBot(cfg *config.Config, log *logging.Logger, mgr *proxy.Manager,
	bans *security.Bans, store *storage.Store) *Bot {
	return &Bot{
		cfg:      cfg,
		log:      log,
		mgr:      mgr,
		bans:     bans,
		store:    store,
		limiter:  security.NewLimiter(cfg.Telegram.RateLimitPerMin, time.Minute),
		notifCh:  make(chan string, 256),
		done:     make(chan struct{}),
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
	api, err := tgbotapi.NewBotAPI(b.cfg.Telegram.BotToken)
	if err != nil {
		return fmt.Errorf("telegram login: %w", err)
	}
	b.api = api
	api.Debug = false
	b.log.Info("telegram bot connected", "user", api.Self.UserName)

	go b.notifyLoop()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = b.cfg.Telegram.PollTimeoutSecond
	updates := api.GetUpdatesChan(u)

	go func() {
		<-ctx.Done()
		api.StopReceivingUpdates()
		close(b.done)
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
	defer close(b.done)
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
		b.log.Info("non-admin blocked", "from", uid, "text", msg.Text)
		return
	}
	if !b.limiter.Allow(strconv.FormatInt(uid, 10)) {
		b.reply(msg, "вЏі <b>Rate limit</b>. РџРѕРґРѕР¶РґРё РЅРµРјРЅРѕРіРѕ Рё РїРѕРїСЂРѕР±СѓР№ СЃРЅРѕРІР°.")
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
		b.send(msg, "РќРµРёР·РІРµСЃС‚РЅР°СЏ РєРѕРјР°РЅРґР°. /start вЂ” СЃРїРёСЃРѕРє РєРѕРјР°РЅРґ.")
	}
}

// ---------- command handlers ----------------------------------------------

func (b *Bot) cmdList(msg *tgbotapi.Message) {
	list := b.mgr.List()
	if len(list) == 0 {
		b.send(msg, "Р‘СЌРєРµРЅРґРѕРІ РЅРµС‚. ` /add <name> <port> <tcp> [udp] `")
		return
	}
	var sb strings.Builder
	sb.WriteString("рџ“¦ <b>Р‘СЌРєРµРЅРґС‹</b>\n")
	sb.WriteString("<code>ID  Name     TCP:в†’backend              UDP       Conns  Up     Tx</code>\n")
	for _, bk := range list {
		m := bk.Model()
		udp := "вЂ”"
		if m.UDPEnabled {
			udp = fmt.Sprintf("%dв†’%s", m.UDPPort, m.BackendUDP)
		}
		sb.WriteString(fmt.Sprintf(
			"<code>%-3d %-8s %-24s %-14s %-5d %-7s %-8s</code>\n",
			m.ID, m.Name, fmt.Sprintf("%dв†’%s", m.ListenPort, m.BackendTCP), udp,
			bk.ActiveConns(), shortUptime(time.Since(bk.StartedAt())), humanBytes(bkStatsTx(bk)),
		))
	}
	b.send(msg, sb.String())
}

func (b *Bot) cmdAdd(msg *tgbotapi.Message, args []string) {
	if len(args) < 3 || len(args) > 4 {
		b.send(msg, "Р¤РѕСЂРјР°С‚: <code>/add &lt;name&gt; &lt;port&gt; &lt;tcp&gt; [udp]</code>")
		return
	}
	name := args[0]
	port, err := strconv.Atoi(args[1])
	if err != nil {
		b.send(msg, "РџРѕСЂС‚ РґРѕР»Р¶РµРЅ Р±С‹С‚СЊ С‡РёСЃР»РѕРј.")
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
		b.send(msg, "вќЊ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/add", name)
	b.send(msg, "вњ… Р‘СЌРєРµРЅРґ РґРѕР±Р°РІР»РµРЅ: <b>"+bk.Name()+"</b>\nTCP "+bk.Model().BackendTCP)
}

func (b *Bot) cmdRemove(msg *tgbotapi.Message, args []string) {
	if len(args) != 1 {
		b.send(msg, "Р¤РѕСЂРјР°С‚: <code>/remove &lt;name|id&gt;</code>")
		return
	}
	ref := args[0]
	// confirm via inline button
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("вљ пёЏ РЈРґР°Р»РёС‚СЊ", "del:"+ref),
			tgbotapi.NewInlineKeyboardButtonData("РћС‚РјРµРЅР°", "cancel"),
		),
	)
	m := tgbotapi.NewMessage(msg.Chat.ID, "РЈРґР°Р»РёС‚СЊ Р±СЌРєРµРЅРґ <b>"+ref+"</b>?")
	m.ParseMode = "HTML"
	m.ReplyMarkup = kb
	_, _ = b.api.Send(m)
}

func (b *Bot) cmdRestart(msg *tgbotapi.Message, args []string) {
	if len(args) != 1 {
		b.send(msg, "Р¤РѕСЂРјР°С‚: <code>/restart &lt;name&gt;</code>")
		return
	}
	if err := b.mgr.Restart(args[0]); err != nil {
		b.send(msg, "вќЊ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/restart", args[0])
	b.send(msg, "рџ”„ Р‘СЌРєРµРЅРґ <b>"+args[0]+"</b> РїРµСЂРµСЃРѕР·РґР°РЅ.")
}

func (b *Bot) cmdAddUDP(msg *tgbotapi.Message, args []string) {
	if len(args) != 2 {
		b.send(msg, "Р¤РѕСЂРјР°С‚: <code>/add-udp &lt;name&gt; &lt;udp&gt;</code>")
		return
	}
	udpPort := findPortFrom(args[1])
	if err := b.mgr.AddUDP(args[0], args[1], udpPort); err != nil {
		b.send(msg, "вќЊ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/add-udp", args[0])
	b.send(msg, "вњ… UDP РїРѕРґРєР»СЋС‡РµРЅ Рє <b>"+args[0]+"</b> ("+args[1]+")")
}

func (b *Bot) cmdRemoveUDP(msg *tgbotapi.Message, args []string) {
	if len(args) != 1 {
		b.send(msg, "Р¤РѕСЂРјР°С‚: <code>/remove-udp &lt;name&gt;</code>")
		return
	}
	if err := b.mgr.RemoveUDP(args[0]); err != nil {
		b.send(msg, "вќЊ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/remove-udp", args[0])
	b.send(msg, "вњ… UDP РѕС‚РєР»СЋС‡РµРЅ Сѓ <b>"+args[0]+"</b>")
}

func (b *Bot) cmdStats(msg *tgbotapi.Message, args []string) {
	if len(args) == 1 {
		bk, ok := b.mgr.Get(args[0])
		if !ok {
			b.send(msg, "Р‘СЌРєРµРЅРґ РЅРµ РЅР°Р№РґРµРЅ.")
			return
		}
		b.send(msg, b.formatBackendStats(bk))
		return
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("рџ”„ РћР±РЅРѕРІРёС‚СЊ", "refresh:all"),
		),
	)
	m := tgbotapi.NewMessage(msg.Chat.ID, b.formatGlobalStats())
	m.ParseMode = "HTML"
	m.ReplyMarkup = kb
	_, _ = b.api.Send(m)
}

func (b *Bot) cmdBan(msg *tgbotapi.Message, args []string) {
	if len(args) < 1 {
		b.send(msg, "Р¤РѕСЂРјР°С‚: <code>/ban &lt;ip&gt; [РїСЂРёС‡РёРЅР°]</code>")
		return
	}
	ip := args[0]
	reason := strings.Join(args[1:], " ")
	if err := b.bans.Ban(ip, reason, strconv.FormatInt(msg.From.ID, 10)); err != nil {
		b.send(msg, "вќЊ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/ban", ip+" "+reason)
	b.send(msg, "рџ”Ё IP <b>"+ip+"</b> Р·Р°Р±Р°РЅРµРЅ: "+reason)
}

func (b *Bot) cmdUnban(msg *tgbotapi.Message, args []string) {
	if len(args) != 1 {
		b.send(msg, "Р¤РѕСЂРјР°С‚: <code>/unban &lt;ip&gt;</code>")
		return
	}
	if err := b.bans.Unban(args[0]); err != nil {
		b.send(msg, "вќЊ "+err.Error())
		return
	}
	_ = b.store.RecordAudit(msg.From.ID, "/unban", args[0])
	b.send(msg, "вњ… IP <b>"+args[0]+"</b> СЂР°Р·Р±Р°РЅРµРЅ.")
}

func (b *Bot) cmdBans(msg *tgbotapi.Message) {
	list, err := b.bans.List()
	if err != nil {
		b.send(msg, "вќЊ "+err.Error())
		return
	}
	b.send(msg, "рџ”’ <b>Р‘Р°РЅС‹</b>\n"+security.Describe(list, 30))
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
		b.send(msg, "Р›РѕРі С„Р°Р№Р» РЅРµ РЅР°СЃС‚СЂРѕРµРЅ.")
	}
}

// ---------- stats formatting ----------------------------------------------

func (b *Bot) formatGlobalStats() string {
	var sb strings.Builder
	sb.WriteString("рџ“€ <b>РћР±С‰Р°СЏ СЃС‚Р°С‚РёСЃС‚РёРєР°</b>\n")
	totalTCP, totalUDP := int64(0), int64(0)
	for _, bk := range b.mgr.List() {
		in, out, udp, _, _ := bk.Stats()
		totalTCP += in + out
		totalUDP += udp
	}
	sb.WriteString(fmt.Sprintf("вЂў <b>TCP</b> РІСЃРµРіРѕ: %s\n", humanBytes(totalTCP)))
	sb.WriteString(fmt.Sprintf("вЂў <b>UDP</b> РІСЃРµРіРѕ: %s\n", humanBytes(totalUDP)))
	sb.WriteString("\nрџЏ† <b>РўРѕРї-5 Р±СЌРєРµРЅРґРѕРІ РїРѕ С‚СЂР°С„РёРєСѓ</b>\n")
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
		sb.WriteString(fmt.Sprintf("%d. <b>%s</b> вЂ” %s\n", i+1, rows[i].name, humanBytes(rows[i].v)))
	}
	return sb.String()
}

func (b *Bot) formatBackendStats(bk *proxy.Backend) string {
	m := bk.Model()
	in, out, udp, pkts, conns := bk.Stats()
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("рџ“Љ <b>%s</b> (#%d)\n", m.Name, m.ID))
	sb.WriteString(fmt.Sprintf("TCP: <code>%d в†’ %s</code> (%s)\n", m.ListenPort, m.BackendTCP, enabledStr(m.Enabled)))
	udpLine := "вЂ”"
	if m.UDPEnabled {
		udpLine = fmt.Sprintf("<code>%d в†’ %s</code>", m.UDPPort, m.BackendUDP)
	}
	sb.WriteString("UDP: " + udpLine + "\n")
	sb.WriteString(fmt.Sprintf("РђРєС‚РёРІРЅС‹С… TCP: %d\n", bk.ActiveConns()))
	sb.WriteString(fmt.Sprintf("РЎРѕРµРґРёРЅРµРЅРёР№ РІСЃРµРіРѕ: %d\n", conns))
	sb.WriteString(fmt.Sprintf("TCP в†‘ %s / в†“ %s\n", humanBytes(out), humanBytes(in)))
	sb.WriteString(fmt.Sprintf("UDP в†‘ %s (%d РїР°РєРµС‚РѕРІ)\n", humanBytes(udp), pkts))
	sb.WriteString(fmt.Sprintf("РђРїС‚Р°Р№Рј: %s", shortUptime(time.Since(bk.StartedAt()))))
	return sb.String()
}

// ---------- callback -------------------------------------------------------

func (b *Bot) handleCallback(q *tgbotapi.CallbackQuery) {
	uid := q.From.ID
	if !b.cfg.AdminAllowed(uid) {
		b.answer(q, "С‚РѕР»СЊРєРѕ РґР»СЏ Р°РґРјРёРЅРѕРІ")
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
		b.answer(q, "РѕС‚РјРµРЅРµРЅРѕ")
	case strings.HasPrefix(data, "del:"):
		ref := strings.TrimPrefix(data, "del:")
		if err := b.mgr.Remove(ref); err != nil {
			b.answer(q, "РѕС€РёР±РєР°: "+err.Error())
			return
		}
		_ = b.store.RecordAudit(uid, "/remove", ref)
		b.editText(chatID, q.Message.MessageID, "рџ—‘ Р‘СЌРєРµРЅРґ <b>"+ref+"</b> СѓРґР°Р»С‘РЅ.")
		b.answer(q, "СѓРґР°Р»РµРЅРѕ")
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

<b>РЈРїСЂР°РІР»РµРЅРёРµ</b>
/list вЂ” С‚Р°Р±Р»РёС†Р° Р±СЌРєРµРЅРґРѕРІ
/add <b>name port tcp</b> [udp] вЂ” РґРѕР±Р°РІРёС‚СЊ
/add-udp <b>name udp</b> вЂ” РґРѕР±Р°РІРёС‚СЊ UDP
/remove-udp <b>name</b> вЂ” РѕС‚РєР»СЋС‡РёС‚СЊ UDP
/restart <b>name</b> вЂ” РїРµСЂРµСЃРѕР·РґР°С‚СЊ listener
/remove <b>name|id</b> вЂ” СѓРґР°Р»РёС‚СЊ

<b>РЎС‚Р°С‚РёСЃС‚РёРєР°</b>
/stats вЂ” РѕР±С‰Р°СЏ
/stats <b>name</b> вЂ” РїРѕ Р±СЌРєРµРЅРґСѓ

<b>Р‘РµР·РѕРїР°СЃРЅРѕСЃС‚СЊ</b>
/ban <b>ip</b> [reason]
/unban <b>ip</b>
/bans

<b>РџСЂРѕС‡РµРµ</b>
/log <b>N</b> вЂ” РїРѕСЃР»РµРґРЅРёРµ N СЃС‚СЂРѕРє Р»РѕРіР°`
