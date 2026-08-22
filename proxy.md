# Контекст и предыстория

Я владелец Minecraft-проекта с бэкенд-сервером на VDS в Германии (хостинг Phylex,
AS213905 ISPLABS, реселлер Hetzner, Ryzen 9 7950X3D, 48GB RAM). Игроки подключаются
из России без VPN. Столкнулся с проблемой: через 5 минут после начала игры пинг
у игроков вырастает с 40мс до 500-1000мс из-за перегруженности магистральных
каналов РФ→Германия (Bufferbloat + транзитные потери).

Решение: поднять собственный гибридный TCP/UDP-прокси на VDS в России.
- TCP-трафик (игра) должен передавать реальные IP игроков на бэкенд через Proxy Protocol v2.
- UDP-трафик (Voice Chat моды: Simple Voice Chat, Plasmo Voice) должен проксироваться
  прозрачно. Для UDP PPv2 не используется, так как голосовые моды идентифицируют игроков
  по UUID из TCP-сессии, а не по IP.
- На стороне бэкенда (Minecraft сервер, ванильное ядро без плагинов) должен использоваться
  Java Agent, который парсит PPv2 заголовок и подменяет remote address в сетевом менеджере
  Minecraft. Этот агент также является частью задачи.

# Задача

Разработать два компонента:
1.  **Go-приложение**: Гибридный TCP/UDP-прокси с управлением через Telegram-бота,
    поддерживающий динамическое добавление/удаление бэкендов, статистику и защиту.
2.  **Java Agent**: Байткод-трансформер для ванильного Minecraft сервера (любая версия/ядро),
    обеспечивающий поддержку Proxy Protocol v2 без установки плагинов/модов.

У меня несколько VDS в Германии с разными IP и портами. Нужна мульти-бэкендная архитектура.

# Технический стек

## Go Proxy
- Язык: Go 1.21+
- Telegram: `go-telegram-bot-api/telegram-bot-api` или `gotd/td`
- Хранение состояния: **SQLite** (драйвер `modernc.org/sqlite`, pure Go, без CGO).
  JSON НЕ использовать.
- Протокол TCP: Proxy Protocol v2
- Протокол UDP: Прозрачный форвардинг (raw UDP)
- Деплой: Linux systemd-сервис

## Java Agent
- Язык: Java 8+ (для совместимости со старыми версиями MC)
- Байткод-манипуляция: `org.javassist:javassist:3.30.2-GA`
- Сборка: Maven Shade Plugin (fat-jar)
- Манифест: `Premain-Class`, `Can-Retransform-Classes: true`

# Функциональные требования: Go Proxy

## 1. Проксирование трафика

### TCP (Игровой трафик)
- Принимает TCP-подключения на порты, привязанные к бэкендам
- Перед передачей данных отправляет PPv2 заголовок с реальным IP и портом игрока
- Двусторонний стриминг с буфером 32KB
- Idle timeout 30 минут

### UDP (Voice Chat трафик)
- Опциональный UDP-порт для каждого бэкенда
- Сессионный маппинг по client IP:port
- Таймаут неактивности UDP-сессии: 60 секунд
- Прозрачная передача пакетов без модификации
- Статистика UDP-трафика отдельно от TCP

### Общие
- Graceful shutdown (drain активных TCP-соединений)
- Hot-reload: изменение бэкендов без перезапуска сервиса

## 2. Telegram-бот (админка)

Доступ только для whitelist Telegram ID.

Команды:
- `/start` — приветствие и список команд
- `/list` — таблица бэкендов (ID, имя, TCP port, UDP status, connections, uptime, traffic)
- `/add <name> <listen_port> <backend_tcp> [backend_udp]` — добавить бэкенд.
  Пример: `/add survival 25565 193.23.221.21:25565 193.23.221.21:24454`
  Если `backend_udp` не указан — создаётся только TCP-бэкенд.
- `/add-udp <name> <backend_udp>` — добавить UDP к существующему TCP-бэкенду
- `/remove-udp <name>` — отключить UDP для бэкенда
- `/remove <name_or_id>` — удалить бэкенд с graceful drain
- `/restart <name>` — пересоздать listener
- `/stats` — общая статистика (TCP+UDP раздельно, топ-5 игроков по трафику)
- `/stats <name>` — детальная статистика по бэкенду
- `/ban <ip> [reason]` — забанить IP (internal ACL + iptables)
- `/unban <ip>`
- `/bans` — список банов
- `/log <N>` — последние N строк лога

Callback-кнопки для подтверждения удаления и refresh статистики.

## 3. Логирование и мониторинг

- Structured logs (JSON): timestamp, event, backend, real_ip, protocol, bytes, duration
- Отдельный access.log для каждого бэкенда (TCP и UDP раздельно)
- Метрики: connections_total, udp_packets_total, errors_total (по типам)
- Уведомления в Telegram:
  - Backend недоступен (3 ошибки dial подряд)
  - DDoS-активность (>100 TCP conn/min или >1000 UDP pkt/min с одного IP)
  - Сервис перезапущен

## 4. Безопасность

- Whitelist admin IDs
- Rate limiting команд (10/мин)
- Backend whitelist (защита от SSRF)
- Max TCP connections per IP: 3
- Max UDP packets per IP per sec: 100
- Audit log всех действий админов

## 5. Конфигурация (YAML)

```yaml
telegram:
  bot_token: "123456:ABC-DEF..."
  admin_ids: [123456789]
  rate_limit_per_minute: 10

proxy:
  default_idle_timeout: "30m"
  udp_session_timeout: "60s"
  buffer_size: 32768
  listen_interface: "0.0.0.0"

storage:
  path: "/var/lib/mc-proxy/state.db"

logging:
  level: "info"
  file: "/var/log/mc-proxy/proxy.log"
  access_log_dir: "/var/log/mc-proxy/access/"

security:
  backend_whitelist: []
  max_tcp_connections_per_ip: 3
  max_udp_packets_per_ip_per_sec: 100
```

## 6. Персистентность (SQLite)

Таблицы: `backends` (с полями udp_listen_port, udp_backend_addr), `bans`, `stats_hourly`, `audit_log`.
Активные соединения хранятся только в памяти.

# Функциональные требования: Java Agent

## 1. Назначение
Перехватывать чтение из сетевого канала Minecraft сервера, считывать Proxy Protocol v2
заголовок ДО передачи данных движку, и подменять RemoteAddress соединения на реальный IP игрока.

## 2. Совместимость
- Любое ядро: Vanilla, Paper, Spigot, Fabric, Forge, Folia
- Любая версия: 1.7.10 – 1.21+
- Без плагинов, модов, конфигурационных файлов

## 3. Механизм работы
- Использовать Java Instrumentation API (`premain`)
- Трансформировать классы сетевого менеджера:
  - `net.minecraft.network.Connection` (1.20.2+)
  - `net.minecraft.server.network.NetworkManager` (1.18-1.20.1)
  - `net.minecraft.network.NetworkManager` (1.7-1.17)
- Вставлять код чтения PPv2 в начало метода `channelActive` / `channelRead`
- Парсить PPv2 header (signature, version, family, addresses)
- Через reflection находить поле `address`/`socketAddress` в NetworkManager и подменять его
- Если PPv2 заголовок отсутствует (прямое подключение) — работать прозрачно без изменений

## 4. Требования к реализации
- Зависимость только на javassist (вшивается в fat-jar через maven-shade-plugin)
- Корректная работа с обфусцированными именами полей (поиск по типу InetSocketAddress)
- Логирование в stdout с префиксом `[MC-PP-Agent]`
- Обработка ошибок без падения сервера (если PPv2 невалиден — игнорировать)

# Архитектурные требования (Go)

- Чистая структура: cmd/, internal/, pkg/
- Интерфейсы: Proxy, Backend, Storage, Notifier, UDPForwarder
- Context propagation + errgroup
- Deps: telegram-lib + modernc.org/sqlite only
- systemd unit
- README с инструкциями

# Требования к коду

- Idiomatic Go / Clean Java
- Полная обработка ошибок
- Unit-тесты: PPv2 builder, UDP session mapper, TG command parser, Java Agent transformer
- Комментарии на русском или английском

# Что НЕ нужно

- GUI/веб-панель
- Bedrock UDP (только Java TCP + Voice Chat UDP)
- Внешние сервисы (TCPShield, NeoProtect)
- JSON для хранения состояния
- Плагины/моды для поддержки PPv2 на бэкенде

# Результат

Предоставь полный исходный код обоих проектов:

## Go Proxy
1. Структура папок + все .go файлы
2. go.mod
3. config.example.yaml
4. systemd unit (proxygo.service)
5. SQL-миграции для SQLite
6. README.md

## Java Agent
1. pom.xml с maven-shade-plugin
2. Исходники: PPAgent.java, PPTransformer.java, PPHandler.java
3. Инструкция по сборке: `mvn clean package`
4. Пример запуска: `java -javaagent:mc-pp-agent-1.0.0.jar -jar server.jar`
5. Таблица совместимости с версиями MC

В конце — объяснение архитектурных решений и список известных ограничений.