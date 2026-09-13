# AGENTS.md

## Что это

`proxygo` — гибридный TCP/UDP-прокси для Minecraft на Go с Telegram-админкой
и состоянием в SQLite. TCP поддерживает три режима форвардинга
(`Backend.ForwardMode`): `raw` (прозрачная труба), `bungee` (переписывает
MC-handshake: `host\0IP\0UUID` — реальный IP игрока на Spigot/Paper с
`bungeecord: true`) и `ppv2` (PROXY v2 заголовок для Paper/Velocity).
UDP — всегда прозрачно.

## Сборка и проверка

```bash
go build -trimpath -ldflags="-s -w" -o proxygo ./cmd/proxygo   # бинарь
go vet ./...                                                  # линт/проверка
go build ./...                                                # сборка всех пакетов
go test ./...                                                 # юнит + e2e (internal/proxy, pkg/mcproto)
```

## Ручная проверка прокси

```bash
cp config.example.yaml config.yaml   # поправить storage.path и logging под свою папку
./proxygo -config config.yaml        # или добавить бэкенд через Telegram-бота
# эмуляция: nc -l -p 25565 (бэкенд) + подключение на listen-порт прокси
```

## Структура

```
cmd/proxygo         main + админ-подкоманды (backends/bans/stats) + wiring/signal
internal/config     YAML-конфиг + валидация + backend-whitelist
internal/logging    slog (json/console) + access-логи + канал уведомлений
internal/metrics    атомарные счётчики + per-IP окна для DDoS-детекта
internal/model      общие типы (Backend, Ban, StatPoint, AuditEntry)
internal/proxy      Manager + Backend (TCP pipe + UDP-сессии + drain + preamble)
internal/security   баны (SQLite + best-effort iptables) + token-bucket limiter
internal/storage    SQLite (modernc.org/sqlite, без CGO) + embed-миграции
internal/telegram   бот: long-polling, команды, inline-кнопки
pkg/mcproto         MC-протокол: frame-reader, handshake, login-start, offline-UUID
pkg/ppv2            сборка/парсинг PROXY v2 заголовка
deploy/             proxygo-deploy.sh (установщик), proxygo-build.sh,
                    proxygo (CLI-обёртка над systemd), proxygo.service
```

## Ключевые архитектурные решения

- **TCP**: `Backend.handleConn` — ban-check → per-IP лимит → DDoS-окно →
  dial backend → `forwardPreamble` (raw/bungee/ppv2 по `forward_mode`) →
  два `pipe`-горутина (client↔backend). `countConn` считает байты и
  продлевает idle-дедлайн на каждом read/write. TCP_NODELAY на обоих сокетах.
- **UDP**: `UDPForwarder` — одна listening-точка; на каждый client IP:port —
  сессия с отдельным ephemeral upstream-сокетом (unconnected, не DialUDP —
  обход quirk'ов Windows/loopback). Сессии протухают по `udp_session_timeout`.
- **Graceful drain**: `shutdown(grace)` отменяет контекст, закрывает
  listener'ы, ждёт `active==0` до grace, затем force-close по `conns`-мапе.
- **Хранилище**: одна write-conn (`SetMaxOpenConns(1)`) + WAL +
  busy_timeout — защита от SQLITE_BUSY. Миграции через `schema_migrations`.
- **Бот**: исходящий long-polling на api.telegram.org (входящие порты не
  нужны); опциональный HTTP/SOCKS-прокси через `telegram.proxy`. MTProto-
  прокси для Bot API не работает.

## Конвенции кода

- Go, стандартная библиотека + минимум зависимостей (см. `go.mod`).
- Ошибки: `fmt.Errorf("context: %w", err)`; на горячем пути — счётчики
  `metrics.Err*` вместо лог-спама.
- Конфиг-строки вида "30m"/"60s" парсятся в `config.expand()` → `time.Duration`.
- Логи — структурные (`log.Info("msg", "key", val)`), уведомления админам —
  через `Notifier.Notify` (неблокирующе).

## Деплой

Прод-окружение — Linux + systemd, установка в `/opt/proxygo` через
`deploy/proxygo-deploy.sh` (локальный Go-тулчейн в `/opt/proxygo/.tool`).
Единый CLI — `/usr/local/bin/proxygo` (`start|stop|restart|status|logs|
backends|bans|stats|config|build|update|remove`).

## Ключевые инварианты форвардинга

- `forwardPreamble` (internal/proxy/proxy.go) отрабатывает ДО старта pipe'ов.
  Ошибка преамбулы = фатально для коннекта.
- `bungee`: читаем handshake → intent==2 (login) → читаем login-start → ник →
  offline-UUID (`md5("OfflinePlayer:"+name)`, v3) → host+`\0`+IP+`\0`+UUID.
  UUID совпадает с тем, что offline-сервер посчитал бы сам → идентичность
  игрока сохраняется. Online-mode сервера spoofedUUID игнорируют — безопасно.
- **Фолбэк в raw — обязателен**: любой не-MC/неполный пакет → реплеем сырые
  фреймы и прозрачный pipe. Никогда не ломать коннект из-за парсинга.
- `ppv2` — только если бэкенд понимает PROXY v2 нативно (Paper
  `proxies.proxy-protocol: true`, Velocity). На голом vanilla/Spigot
  сломает handshake — именно поэтому раньше «не проксировалось».
- `mcproto.ReadFrame` читает ровно один фрейм без буферизации — conn
  остаётся валидным для последующего pipe().

## Важно: история с Java-агентом

Раньше проект включал `proxygo-mc-agent` (Java-агент на Javassist) и
посылал каждому бэкенду PROXY Protocol v2 заголовок, чтобы передать
реальный IP игрока. Это была логическая ловушка: **без агента на бэкенде
PPv2-заголовок ломал Minecraft-handshake** — первые байты соединения были
`\r\n\r\n\x00\r\nQUIT\n` вместо MC-пакета, и трафик фактически не
проксировался. Агент удалён; вместо него — protocol-aware режимы
`bungee`/`ppv2` (per-backend, `/forward`), требующие лишь настройки на
стороне сервера, без jar'ов.
