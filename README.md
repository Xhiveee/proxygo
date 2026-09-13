# proxygo

Гибридный TCP/UDP-прокси для Minecraft с Telegram-админкой.

Прокси прозрачно форвардит TCP-соединения и UDP-датаграммы на бэкенд.
Сторонних агентов/плагинов на сервере Minecraft не требуется; бэкенд видит
IP прокси как адрес пира.

```
клиент ──► proxygo ──► Minecraft (бэкенд)
          TCP/UDP прозрачно
```

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
internal/proxy      Manager + TCP + UDP (сессии) + drain
internal/security   баны (SQLite + iptables) + rate-limit
internal/storage    SQLite (modernc.org/sqlite) + миграции
internal/telegram   бот (long-polling), команды, callback-кнопки
```

## Схема БД (SQLite)

`backends`, `bans`, `stats_hourly`, `audit_log` — миграции в
`internal/storage/migrations/001_init.sql`, применяются автоматически
(отслеживаются через `schema_migrations`).

## Известные ограничения

- Бэкенд видит IP прокси, а не реальный IP игрока — серверный бан/whitelist
  по IP и гео-логика на стороне Minecraft работать не будут.
- TCP idle-timeout (30м) применяется к обеим половинам стрима —
  `proxy.default_idle_timeout`.
- UDP-сессии живут `udp_session_timeout`; голос начинается заново после паузы.
- `enforce_iptables` требует root/`CAP_NET_ADMIN`; без него работает внутренний ACL.
- `/log` читает файл лога; при logrotate часть строк может теряться.
