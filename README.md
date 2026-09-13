# proxygo

Гибридный TCP/UDP-прокси для Minecraft с Telegram-админкой и
protocol-aware форвардингом реального IP игрока.

Прокси прозрачно форвардит TCP и UDP, а для передачи реального IP игрока
умеет два режима без агентов/плагинов: **bungee** (переписывает MC-handshake
в формате BungeeCord) и **ppv2** (PROXY protocol v2 для Paper/Velocity).

```
клиент ──► proxygo ──► Minecraft (бэкенд)
          TCP: raw | bungee | ppv2
          UDP: прозрачно
```

## Режимы форвардинга TCP (`/forward <name> <mode>`)

| Режим | Что делает | Что нужно на сервере |
|---|---|---|
| `raw` (default) | Прозрачная труба; бэкенд видит IP прокси | Ничего |
| `bungee` | Переписывает handshake → `host\0IP\0UUID`. Реальный IP игрока | Spigot/Paper: `bungeecord: true` в spigot.yml |
| `ppv2` | Шлёт PROXY v2 заголовок перед данными | Paper: `proxies.proxy-protocol: true` (или Velocity) |

`bungee` читает handshake + login-start, берёт ник и подставляет
offline-UUID — тот же, что сервер посчитал бы сам, поэтому идентичность
игрока не ломается. Status-ping и не-MC трафик проходят как есть.
Всё безопасно: если пакеты не парсятся — фолбэк на прозрачный режим.

## Установка на сервер

```bash
curl -fsSL https://raw.githubusercontent.com/Xhiveee/proxygo/main/deploy/proxygo-deploy.sh | sudo bash
```

Скрипт: клонирует проект в `/opt/proxygo`, ставит Go локально
(`/opt/proxygo/.tool`, не глобально), собирает бинарь, спрашивает
Telegram-токен (пропуск — бот отключён на время), ставит systemd-юнит и CLI.

> Для SSH-клона: `PROXYGO_REPO=git@github.com:Xhiveee/proxygo.git`.

## Быстрый старт

```bash
go build -trimpath -ldflags="-s -w" -o proxygo ./cmd/proxygo
cp config.example.yaml config.yaml   # заполнить bot_token / admin_ids
./proxygo -config config.yaml
```

Проверка:

```bash
go vet ./...
```

### systemd

```bash
sudo mkdir -p /opt/proxygo/data /opt/proxygo/log/access /opt/proxygo/run
sudo cp proxygo /usr/local/bin/proxygo
sudo cp config.yaml /opt/proxygo/config.yaml
sudo cp deploy/proxygo.service /etc/systemd/system/
sudo useradd -r -s /usr/sbin/nologin proxygo || true
sudo systemctl daemon-reload && sudo systemctl enable --now proxygo
```

### Единый CLI

`proxygo` в `/usr/local/bin/proxygo` — единый интерфейс. `proxygo stop` —
то же, что `systemctl stop proxygo` (CLI вызывает systemd, иначе pid-файл).

```bash
proxygo start | stop | restart | status   # сервис
proxygo logs [N]                          # хвост лога
proxygo backends | bans | stats           # состояние из БД
proxygo config | build | update | remove [-y]
```

## Telegram-команды

Доступ только для `admin_ids`, rate-limit 10/мин.

> ⚠️ Бот работает через **исходящее** HTTPS на `api.telegram.org:443` (long-polling,
> входящие порты не нужны). Если Telegram недоступен из сети сервера (блокировка на
> уровне ISP/региона) — укажи **HTTP/SOCKS прокси или VPN** в конфиге:
> `telegram.proxy: "socks5://host:1080"` (или `http://host:8080`). MTProto-прокси для
> Bot API не работает (он только для клиента-мессенджера). После изменения — `proxygo restart`.

```
/start                     список команд
/list                      таблица бэкендов
/add <name> <port> <tcp> [udp]   добавить бэкенд
/add-udp <name> <udp>       прицепить UDP
/remove-udp <name>          отключить UDP
/remove <name|id>           удалить (с подтверждением, graceful drain)
/restart <name>             пересоздать listener
/forward <name> <mode>     режим TCP: raw | bungee | ppv2 (реальный IP)
/stats | /stats <name>      статистика (кнопка refresh)
/ban <ip> [reason]          забанить (SQLite + iptables)
/unban <ip>
/bans                       список банов
/log <N>                    последние N строк лога
```

## Структура (Go)

```
cmd/proxygo        main + CLI-подкоманды + wiring/signal
internal/config     YAML-конфиг + валидация + backend-whitelist
internal/logging    structured JSON-логи + access.log + notify-канал
internal/metrics    атомарные счётчики + per-IP окна (DDoS-детект)
internal/model      общие типы (Backend, Ban, StatPoint, AuditEntry)
internal/proxy      Manager + TCP (raw/bungee/ppv2) + UDP (сессии) + drain
internal/security   баны (SQLite + iptables) + rate-limit
internal/storage    SQLite (modernc.org/sqlite) + миграции
internal/telegram   бот (long-polling), команды, callback-кнопки
pkg/mcproto         MC-протокол: фреймы, handshake, login-start, offline-UUID
pkg/ppv2            сборка/парсинг PROXY v2 заголовка
```

## Схема БД (SQLite)

`backends`, `bans`, `stats_hourly`, `audit_log` — миграции в
`internal/storage/migrations/001_init.sql`, применяются автоматически
(отслеживаются через `schema_migrations`).

## Известные ограничения

- В режиме `raw` бэкенд видит IP прокси — серверный бан/whitelist по IP и
  гео-логика на стороне Minecraft работать не будут. Для реального IP —
  `/forward <name> bungee` (Spigot/Paper) или `ppv2` (Paper/Velocity).
- `bungee` передаёт offline-UUID; в online-mode бэкендах он игнорируется
  (Mojang-авторизация возвращает настоящий) — IP при этом подменяется
  корректно.
- UDP не несёт handshake — реальный IP через UDP не передаётся.
- TCP idle-timeout (30м) применяется к обеим половинам стрима —
  `proxy.default_idle_timeout`.
- UDP-сессии живут `udp_session_timeout`; голос начинается заново после паузы.
- `enforce_iptables` требует root/`CAP_NET_ADMIN`; без него работает внутренний ACL.
- `/log` читает файл лога; при logrotate часть строк может теряться.
