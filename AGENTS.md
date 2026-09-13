# AGENTS.md

## Что это

`proxygo` — гибридный TCP/UDP-прокси для Minecraft на Go с Telegram-админкой
и состоянием в SQLite. Трафик форвардится **прозрачно**: бэкенд видит IP
прокси, а не реальный IP игрока.

## Сборка и проверка

```bash
go build -trimpath -ldflags="-s -w" -o proxygo ./cmd/proxygo   # бинарь
go vet ./...                                                  # линт/проверка
go build ./...                                                # сборка всех пакетов
```

Тестов нет. Проверка изменений: `go build ./... && go vet ./...`, для
сетевых правок — ручной прогон (см. ниже).

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
internal/proxy      Manager + Backend (TCP pipe + UDP-сессии + drain)
internal/security   баны (SQLite + best-effort iptables) + token-bucket limiter
internal/storage    SQLite (modernc.org/sqlite, без CGO) + embed-миграции
internal/telegram   бот: long-polling, команды, inline-кнопки
deploy/             proxygo-deploy.sh (установщик), proxygo-build.sh,
                    proxygo (CLI-обёртка над systemd), proxygo.service
```

## Ключевые архитектурные решения

- **TCP**: `Backend.handleConn` — ban-check → per-IP лимит → DDoS-окно →
  dial backend → два `pipe`-горутина (client↔backend). `countConn` считает
  байты и продлевает idle-дедлайн на каждом read/write.
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

## Важно: история с Java-агентом

Раньше проект включал `proxygo-mc-agent` (Java-агент на Javassist) и
посылал каждому бэкенду PROXY Protocol v2 заголовок, чтобы передать
реальный IP игрока. Это была логическая ловушка: **без агента на бэкенде
PPv2-заголовок ломал Minecraft-handshake** — первые байты соединения были
`\r\n\r\n\x00\r\nQUIT\n` вместо MC-пакета, и трафик фактически не
проксировался. Агент и `pkg/ppv2` удалены; прокси теперь честно
транспарентный. Не возвращать PPv2 без механизма на стороне бэкенда,
который его снимает.
