# MC Hybrid Proxy

Гибридный TCP/UDP-прокси для Minecraft с Telegram-админкой и Java-агентом для
поддержки Proxy Protocol v2 на ванильном бэкенде.

Решает проблему роста пинга (40мс → 500-1000мс через 5 минут) у игроков из РФ,
вызванную перегрузкой магистральных каналов РФ→Германия. Прокси поднимается на
российском VDS: TCP (игра) передаётся с реальным IP игрока через PPv2, UDP
(Voice Chat модов) проксируется прозрачно.

```
Игрок (RU) ──► proxygo (RU VDS) ──► бэкенд (DE VDS)
              │ TCP: + PPv2 header        Java Agent читает PPv2,
              │ UDP: прозрачно            подменяет remote address
```

## Состав

| Компонент | Каталог | Описание |
|---|---|---|
| Go Proxy | `./` | TCP/UDP прокси, Telegram-бот, SQLite, безопасность, метрики |
| Java Agent | `./proxygo-mc-agent` | Байткод-трансформер для чтения PPv2 на сервере |

## Установка на сервер (curl)

Одной командой на свежем Linux VDS (ставятся последний Docker, Go, JDK и Maven
**локально внутри проекта**, компилируются бинарь и Java-агент):

```bash
curl -fsSL https://raw.githubusercontent.com/Xhiveee/proxygo/main/deploy/proxygo-deploy.sh | sudo bash
```

Скрипт сам:
* ставит последний **Docker**, если его нет (пропустить: `PROXYGO_NO_DOCKER=1`);
* клонирует проект в **`/opt/proxygo`**;
* ставит Go/JDK/Maven в `/opt/proxygo/.tool` (только внутри проекта, не глобально);
* собирает бинарь `/opt/proxygo/bin/proxygo` и агент `/opt/proxygo/proxygo-mc-agent/target/proxygo-mc-agent.jar`;
* спрашивает **Telegram-токен и admin_ids** — при пустом ответе бот отключается
  (`telegram.disabled: true`) и токен вписывается вручную в конфиг;
* ставит systemd-юнит `proxygo.service` и CLI `proxygo`, запускает сервис.

> Для SSH-клона: `PROXYGO_REPO=git@github.com:Xhiveee/proxygo.git`. Для docker без
> iptables: `PROXYGO_NO_DOCKER=1`.

## Быстрый старт (Go Proxy)

```bash
# 1. Сборка
go build -trimpath -ldflags="-s -w" -o proxygo ./cmd/proxygo

# 2. Конфиг
cp config.example.yaml config.yaml   # указать bot_token, admin_ids и т.д.

# 3. Запуск
./proxygo -config config.yaml
```

Проверка тестов и линтера:

```bash
go test ./...
go vet ./...
```

### systemd (Linux)

```bash
sudo mkdir -p /opt/proxygo/data /opt/proxygo/log/access /opt/proxygo/run
sudo cp proxygo /usr/local/bin/proxygo
sudo cp config.yaml /opt/proxygo/config.yaml
sudo cp deploy/proxygo.service /etc/systemd/system/
sudo useradd -r -s /usr/sbin/nologin proxygo || true
sudo systemctl daemon-reload && sudo systemctl enable --now proxygo
```

Управление: `proxygo start | stop | restart | status | logs | remove`.

### Docker

```bash
docker build -t proxygo .
docker run -d --name proxygo --cap-add=NET_ADMIN \
  -v /opt/proxygo/data:/opt/proxygo/data \
  -v /opt/proxygo/log:/opt/proxygo/log \
  -p 80-9000:80-9000/udp -p 80-9000:80-9000/tcp proxygo
```

> `--cap-add=NET_ADMIN` нужен только если `security.enforce_iptables: true`.

## Telegram-команды

Доступ только для `admin_ids` из конфига, с rate-limit 10/мин.

```
/start                      список команд
/list                       таблица бэкендов
/add <name> <port> <tcp> [udp]   добавить бэкенд
/add-udp <name> <udp>       прицепить UDP к TCP-бэкенду
/remove-udp <name>          отключить UDP
/remove <name|id>           удалить (с confirm-кнопкой, graceful drain)
/restart <name>             пересоздать listener
/stats | /stats <name>      общая / по бэкенду (кнопка refresh)
/ban <ip> [reason]          забанить (SQLite + iptables)
/unban <ip>
/bans                       список банов
/log <N>                    последние N строк лога
```

Пример:

```
/add survival 25565 193.23.221.21:25565 193.23.221.21:24454
/add-udp survival 193.23.221.21:24454
```

## Структура (Go)

```
cmd/proxygo        main + wiring/signal handling
internal/config     YAML-конфиг + валидация + whitelist
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
`internal/storage/migrations/001_init.sql` (применяются автоматически,
отслеживаются через `schema_migrations`).

---

# Java Agent (proxygo-mc-agent)

Байткод-трансформер на Javassist, который перехватывает первый входящий фрейм
сетевого менеджера Minecraft, читает PPv2 заголовок и подменяет remote address
соединения. Не требует плагинов/модов/конфиг-файлов.

### Сборка

```bash
cd proxygo-mc-agent
mvn clean package
# → target/proxygo-mc-agent.jar (fat-jar, javassist зашит внутрь)
```

### Запуск

```bash
java -javaagent:proxygo-mc-agent/target/proxygo-mc-agent.jar -jar server.jar nogui
```

Мониторинг в консоли: строки с префиксом `[proxygo-agent]`.

### Как это работает

1. `Premain` регистрирует `PPTransformer`.
2. Трансформер находит класс сетевого менеджера
   (`net.minecraft.network.Connection` / `NetworkManager`) и вставляет вызов
   `PPHandler.handle($0,$1,$2)` в начало `channelRead`/`channelRead0`.
3. `PPHandler`: если байфуф начинается с сигнатуры `0x0D0A...`, парсит
   версию/команду/семейство, вынимает реальный IP:порт, рефлексией находит
   поле типа `InetSocketAddress` и пишет в него адрес, затем `skipBytes` снимает
   заголовок — движок видит только Minecraft-протокол.
4. Если сигнатуры нет (прямое подключение) — обработчик просто возвращается,
   агент полностью прозрачен.

При обнаружении поля по типу, а не по имени, обфускация/переименования не мешают.

### Таблица совместимости

| Версия MC | Класс сети | Класс после трансформа |
|---|---|---|
| 1.7.10 – 1.17 | `net.minecraft.network.NetworkManager` | читает PPv2, подмена address |
| 1.18 – 1.20.1 | `net.minecraft.server.network.NetworkManager` | то же |
| 1.20.2 – 1.21+ | `net.minecraft.network.Connection` | то же (поле socketAddress) |
| Vanilla / Paper / Spigot | сохраняют MCP-имена | поддерживается |
| Fabric / Forge / Folia | своя загрузка классов | поддерживается (по имени) |

### Тесты

```bash
cd proxygo-mc-agent && mvn test
```

Проверяются: парсинг PPv2 с подстановкой реального адреса и снятием заголовка,
прозрачность при прямом подключении, частичный заголовок и сам трансформер.

## Известные ограничения

- TCP idle-timeout применяется к обеим половинам стрима; при 30м простое
  соединение закроется (настраивается `proxy.default_idle_timeout`).
- UDP-сессии живут `udp_session_timeout`; голос начинает заново после паузы.
- PPv2-заголовок должен приходить одним TCP-сегментом (в норме да, но при
  крайней сегментации агент ждёт следующего фрейма).
- `/log` читает файл лога; при повёрнутых логах (logrotate) часть строк может
  теряться.
- Агент меняет `InetSocketAddress` рефлексией; на Java 17+ с жёсткими модулями
  (редкий кейс для серверов на classpath) поле может быть недоступно.
- Баны в iptables требуют `CAP_NET_ADMIN`/root; без них работает только
  внутренний ACL (SQLite).
