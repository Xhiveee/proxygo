# proxygo-mc-agent

Java-агент для ванильного Minecraft-сервера (или любого ядра), который читает
HAProxy PROXY v2 заголовок и подменяет remote address соединения реальным IP
игрока. Без плагинов, модов и конфигов.

## Сборка

Требуется JDK 8+ и Maven 3.6+.

```bash
mvn clean package
# → target/proxygo-mc-agent-1.0.0.jar (fat-jar)
```

## Запуск

```bash
java -javaagent:target/proxygo-mc-agent-1.0.0.jar -jar server.jar nogui
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
