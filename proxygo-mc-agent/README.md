# proxygo-mc-agent

Java-агент для **бэкенд-серверов Minecraft в Германии** (не для РФ-сервера с
proxygo). Читает HAProxy PROXY v2 заголовок и подменяет remote address
соединения реальным IP игрока. Без плагинов, модов и конфигов. Ставится
**отдельно на каждый немецкий сервер** — jar копируется в директорию этого
сервера.

> ⚠️ Агент работает на каждом бэкенд-сервере Minecraft (в Германии), а сам
> proxygo (Go) — на сервере в РФ. На РФ-сервере лишь собирается этот jar.

## Сборка

Требуется JDK 8+ и Maven 3.6+ (на РФ-сервере это делает `proxygo build`).

```bash
mvn clean package
# → target/proxygo-mc-agent.jar (fat-jar)
```

## Установка на сервер Minecraft (в Германии)

1. Скопируй `target/proxygo-mc-agent.jar` на немецкий сервер в директорию сервера.
2. Добавь агент в запуск:

```bash
java -javaagent:/путь/к/серверу/proxygo-mc-agent.jar -jar server.jar nogui
```

Логи пишутся в stdout с префиксом `[proxygo-agent]`:

```
[proxygo-agent] agent loaded (PPv2 rewriter active)
[proxygo-agent] instrumented net.minecraft.network.Connection (41823 bytes)
[proxygo-agent] substituted Connection#address -> 203.0.113.5:51234
```

## Совместимость

| Версия MC | Сетевой класс | Поле для подмены |
|---|---|---|
| 1.7.10 – 1.17 | `net.minecraft.network.NetworkManager` | `InetSocketAddress` |
| 1.18 – 1.20.1 | `net.minecraft.server.network.NetworkManager` | `InetSocketAddress` |
| 1.20.2 – 1.21+ | `net.minecraft.network.Connection` | `SocketAddress`/`InetSocketAddress` |

Работает на Vanilla, Paper, Spigot, Fabric, Forge, Folia. Поле ищется по типу, а не
по имени, поэтому обфускация не мешает. При отсутствии PPv2-заголовка (прямое
подключение) агент прозрачен.

## Тесты

```bash
mvn test
```

## Структура

```
PPAgent        premain/agentmain, регистрирует трансформер
PPTransformer  ищет сетевой класс и встраивает вызов PPHandler.handle
PPHandler      парсит PPv2, подменяет адрес, снимает заголовок
```
