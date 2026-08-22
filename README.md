# proxygo

Гибридный TCP/UDP-прокси для Minecraft с Telegram-админкой и Java-агентом для
поддержки Proxy Protocol v2 (PPv2).

Прокси передаёт реальный IP игрока на бэкенд через PPv2 (TCP) и прозрачно
форвардит UDP. Java-агент на сервере читает PPv2 и подменяет remote address
соединения — без плагинов/модов.

```
клиент ──► proxygo ──► Minecraft (бэкенд)
          │ TCP: + PPv2        Java Agent читает PPv2,
          │ UDP: прозрачно     подменяет remote address
```

## Состав

| Компонент | Каталог | Описание |
|---|---|---|
| Go Proxy | `./` | TCP/UDP прокси, Telegram-бот, SQLite, безопасность, метрики |
| Java Agent | `./proxygo-mc-agent` | Байткод-трансформер для чтения PPv2 на сервере |

## Установка на сервер

```bash
curl -fsSL https://raw.githubusercontent.com/Xhiveee/proxygo/main/deploy/proxygo-deploy.sh | sudo bash
```

Скрипт: клонирует проект в `/opt/proxygo`, ставит Go/JDK/Maven локально
(`/opt/proxygo/.tool`, не глобально), собирает бинарь и Java-агент, спрашивает
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
internal/proxy      Manager + TCP (PPv2) + UDP (сессии) + drain
internal/security   баны (SQLite + iptables) + rate-limit
internal/storage    SQLite (modernc.org/sqlite) + миграции
internal/telegram   бот (long-polling), команды, callback-кнопки
pkg/ppv2            сборка/парсинг PROXY v2 заголовка
```

## Схема БД (SQLite)

`backends`, `bans`, `stats_hourly`, `audit_log` — миграции в
`internal/storage/migrations/001_init.sql`, применяются автоматически
(отслеживаются через `schema_migrations`).

---

# Java Agent (proxygo-mc-agent)

Байткод-трансформер на Javassist: перехватывает первый входящий фрейм сетевого
менеджера Minecraft, читает PPv2 заголовок и подменяет remote address. Устанавливается
на каждый сервер Minecraft (бэкенд), а не на хост с proxygo — там jar достаточно
собрать и скопировать.

### Сборка

```bash
cd proxygo-mc-agent
mvn clean package
# → target/proxygo-mc-agent.jar (fat-jar, javassist зашит внутрь)
```

### Установка на сервер Minecraft

1. Скопируй `proxygo-mc-agent.jar` на сервер в директорию сервера (рядом с `server.jar`).
2. Запускай, указав агент просто именем jar:

```bash
java -javaagent:proxygo-mc-agent.jar -jar server.jar nogui
```

Мониторинг: строки с префиксом `[proxygo-agent]`.

### Как это работает

1. `Premain` регистрирует `PPTransformer`.
2. Трансформер находит сетевой класс (`Connection` / `NetworkManager`) и вставляет
   вызов `PPHandler.handle($0,$1,$2)` в начало `channelRead`/`channelRead0`.
3. `PPHandler`: если байфуф начинается с сигнатуры `0x0D0A...`, парсит заголовок,
   рефлексией находит поле типа `InetSocketAddress` и пишет реальный IP:порт, затем
   `skipBytes` снимает заголовок — движок видит только Minecraft-протокол.
4. Если сигнатуры нет (прямое подключение) — прозрачный no-op.

Поле ищется по типу, а не по имени, поэтому обфускация/переименования не мешают.

### Таблица совместимости

| Версия MC | Сетевой класс |
|---|---|
| 1.7.10 – 1.17 | `net.minecraft.network.NetworkManager` |
| 1.18 – 1.20.1 | `net.minecraft.server.network.NetworkManager` |
| 1.20.2 – 1.21+ | `net.minecraft.network.Connection` |

Работает на Vanilla, Paper, Spigot, Fabric, Forge, Folia. Без PPv2-заголовка —
прозрачен.

## Известные ограничения

- TCP idle-timeout (30м) применяется к обеим половинам стрима —
  `proxy.default_idle_timeout`.
- UDP-сессии живут `udp_session_timeout`; голос начинается заново после паузы.
- PPv2-заголовок должен прийти одним TCP-сегментом (иначе агент ждёт следующих байт).
- Агент подменяет `InetSocketAddress` рефлексией; на Java 17+ с жёсткими модулями
  поле может быть недоступно.
- `enforce_iptables` требует root/`CAP_NET_ADMIN`; без него работает внутренний ACL.
- `/log` читает файл лога; при logrotate часть строк может теряться.
