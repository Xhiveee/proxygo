#!/usr/bin/env bash
#
# proxygo-deploy.sh - one-shot installer for the MC Hybrid Proxy.
#
# Run from your server (targets Linux, ideally a VDS with systemd):
#
#   curl -fsSL https://raw.githubusercontent.com/Xhiveee/proxygo/main/deploy/proxygo-deploy.sh | sudo bash
#
# What it does:
#   * clones the project into /opt/proxygo
#   * installs Go, a JDK and Maven *locally inside the project* (/opt/proxygo/.tool)
#     so nothing is installed system-wide for compilation
#   * compiles the Go proxy -> /opt/proxygo/bin/proxygo
#   * compiles the Java agent  -> /opt/proxygo/proxygo-mc-agent/target/proxygo-mc-agent.jar
#   * asks for the Telegram bot token / admin IDs (skippable -> manual config)
#   * installs a systemd unit + the 'proxygo' management CLI and (re)starts it
#
# Override via env:
#   PROXYGO_REPO        repo to clone (default the HTTPS URL; use git@ for SSH)
#   PROXYGO_BRANCH      branch (default main)
#   PROXYGO_GO_VERSION  Go version override
#   PROXYGO_JDK_VERSION JDK version override (default 21)
#   PROXYGO_MAVEN_VERSION Maven version override (default 3.9.9)
#   PROXYGO_HOME        install dir (default /opt/proxygo)

set -euo pipefail

# ------------------------------------------------------------------ config --
INSTALL_DIR="${PROXYGO_HOME:-/opt/proxygo}"
REPO="${PROXYGO_REPO:-https://github.com/Xhiveee/proxygo.git}"
BRANCH="${PROXYGO_BRANCH:-main}"
GO_VERSION="${PROXYGO_GO_VERSION:-}"        # empty -> auto-latest
JDK_VERSION="${PROXYGO_JDK_VERSION:-21}"
MAVEN_VERSION="${PROXYGO_MAVEN_VERSION:-3.9.9}"
BIN="$INSTALL_DIR/bin"
TOOL="$INSTALL_DIR/.tool"

RED=$'\033[1;31m'; GRN=$'\033[1;32m'; YEL=$'\033[1;33m'; B=$'\033[1m'; R=$'\033[0m'
C_CYAN=$'\033[1;36m'; DIM=$'\033[2m'
log() { printf "${B}[proxygo]${R} %s\n" "$*"; }
ok()  { printf "${GRN}+ %s${R}\n" "$*"; }
info(){ printf "${YEL}  %s${R}\n" "$*"; }
die() { printf "${RED}! %s${R}\n" "$*" >&2; exit 1; }

have_systemd() { command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; }

# ------------------------------------------------------------------- steps --
need_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "run as root:  sudo $0   (or: curl ... | sudo bash)"
    fi
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  ARCH=amd64;  JARCH=x64 ;;
        aarch64|arm64) ARCH=arm64;  JARCH=aarch64 ;;
        armv7l|armhf)  ARCH=armv6l; JARCH=arm ;;
        *) die "unsupported arch $(uname -m)" ;;
    esac
    log "arch: $(uname -m) -> go=$ARCH jdk=$JARCH"
}

ensure_basics() {
    local need="curl tar git xz-utils ca-certificates"
    command -v apt-get >/dev/null && { export DEBIAN_FRONTEND=noninteractive; apt-get update -y >/dev/null 2>&1 || true; apt-get install -y $need >/dev/null 2>&1 || true; }
    command -v dnf     >/dev/null && { dnf install -y $need >/dev/null 2>&1 || true; }
    command -v yum     >/dev/null && { yum install -y $need >/dev/null 2>&1 || true; }
    for c in curl tar git; do command -v "$c" >/dev/null || die "missing required tool: $c"; done
    ok "system basics ok"
}

ensure_source() {
    if [ -d "$INSTALL_DIR/.git" ]; then
        log "source already present at $INSTALL_DIR (skipping clone)"
        return
    fi
    if [ -e "$INSTALL_DIR" ]; then
        if [ -z "$(ls -A "$INSTALL_DIR" 2>/dev/null)" ]; then rmdir "$INSTALL_DIR"; else die "$INSTALL_DIR exists but is not a git clone"; fi
    fi
    info "cloning $REPO ($BRANCH) -> $INSTALL_DIR"
    git clone --depth 1 --branch "$BRANCH" "$REPO" "$INSTALL_DIR"
    ok "source cloned"
}

latest_go() {
    curl -fsSL "https://go.dev/dl/?mode=json" 2>/dev/null \
        | tr -d '\n' | grep -o '"version": *"go[0-9.]*"' | head -1 | cut -d'"' -f4 | sed 's/^go//'
}

install_go() {
    if [ -x "$TOOL/go/bin/go" ]; then log "Go toolchain already installed"; return; fi
    local ver="$GO_VERSION"; [ -z "$ver" ] && ver="$(latest_go)"
    info "installing Go $ver (project-local: $TOOL/go)"
    curl -fsSL "https://go.dev/dl/go${ver}.linux-${ARCH}.tar.gz" -o /tmp/go.tgz || die "Go download failed"
    mkdir -p "$TOOL"
    tar -C "$TOOL" -xzf /tmp/go.tgz
    ok "Go $ver installed"
}

install_maven() {
    if [ -x "$TOOL/maven/bin/mvn" ]; then log "Maven already installed"; return; fi
    info "installing Maven $MAVEN_VERSION (project-local: $TOOL/maven)"
    local url="https://archive.apache.org/dist/maven/maven-3/${MAVEN_VERSION}/binaries/apache-maven-${MAVEN_VERSION}-bin.tar.gz"
    curl -fsSL "$url" -o /tmp/mvn.tgz || die "Maven download failed"
    mkdir -p "$TOOL/maven.tmp"
    tar -C "$TOOL/maven.tmp" -xzf /tmp/mvn.tgz
    mv "$TOOL/maven.tmp"/* "$TOOL/maven"
    rm -rf "$TOOL/maven.tmp"
    ok "Maven $MAVEN_VERSION installed"
}

install_jdk() {
    if [ -x "$TOOL/jdk/bin/java" ]; then log "JDK already installed"; return; fi
    info "installing Temurin JDK $JDK_VERSION (project-local: $TOOL/jdk)"
    local url="https://api.adoptium.net/v3/binary/latest/${JDK_VERSION}/ga/linux/${JARCH}/jdk/hotspot/normal/eclipse"
    curl -fsSL "$url" -o /tmp/jdk.tgz || { curl -fsSL "$url" -o /tmp/jdk.tgz || die "JDK download failed"; }
    mkdir -p "$TOOL/jdk.tmp"
    tar -C "$TOOL/jdk.tmp" -xzf /tmp/jdk.tgz
    mv "$TOOL/jdk.tmp"/* "$TOOL/jdk"
    rm -rf "$TOOL/jdk.tmp"
    ok "JDK $JDK_VERSION installed"
}

build_sources() {
    mkdir -p "$BIN" "$INSTALL_DIR/data" "$INSTALL_DIR/log/access" "$INSTALL_DIR/run"

    if [ -x "$TOOL/go/bin/go" ]; then
        info "compiling Go proxy ..."
        export GOROOT="$TOOL/go" PATH="$TOOL/go/bin:$PATH" CGO_ENABLED=0
        ( cd "$INSTALL_DIR" && go build -mod=mod -trimpath -ldflags="-s -w" -o "$BIN/proxygo" ./cmd/proxygo )
        ok "Go proxy -> $BIN/proxygo"
    else
        warn_go=1
    fi

    if [ -x "$TOOL/maven/bin/mvn" ] && [ -x "$TOOL/jdk/bin/java" ]; then
        info "compiling Java agent ..."
        export JAVA_HOME="$TOOL/jdk" PATH="$TOOL/jdk/bin:$TOOL/maven/bin:$PATH"
        ( cd "$INSTALL_DIR/proxygo-mc-agent" && mvn -q -Dmaven.test.skip=true clean package )
        local jar="$INSTALL_DIR/proxygo-mc-agent/target/proxygo-mc-agent.jar"
        [ -f "$jar" ] || { info "Java agent build did not produce $jar"; warn_agent=1; return 0; }
        ok "Java agent -> $jar"
    else
        warn_agent=1
    fi
}

# ask reads a value from the controlling terminal (/dev/tty) so the prompt also
# works when the script is piped in (curl ... | sudo bash). Returns 1 if no tty.
ask() {
    local prompt="$1" var="$2" val=""
    if [ -e /dev/tty ] && [ -r /dev/tty ]; then
        printf '%s' "$prompt" > /dev/tty 2>/dev/null || true
        IFS= read -r val < /dev/tty || true
        printf -v "$var" '%s' "$val"
        return 0
    fi
    return 1
}

make_config() {
    local cfg="$INSTALL_DIR/config.yaml"
    [ -f "$cfg" ] && { log "config.yaml exists -> leaving it"; return; }
    cp "$INSTALL_DIR/config.example.yaml" "$cfg"

    local TOKEN="${TELEGRAM_TOKEN:-}" ADMINS="${TELEGRAM_ADMINS:-}" PROXY="${TELEGRAM_PROXY:-}"
    if [ -z "$TOKEN" ]; then
        ask "${B}Telegram bot token${R} (Enter — пропустить, потом впишешь вручную): " TOKEN || true
    fi
    if [ -z "$ADMINS" ]; then
        ask "${B}Telegram admin user IDs${R}, через запятую (например 8622549424 или 8622549424,7777777777): " ADMINS || true
    fi
    if [ -z "$PROXY" ]; then
        ask "${B}Прокси для Telegram${R} (socks5://host:1080 | http://host:8080; Enter — пропустить): " PROXY || true
    fi
    # нормализуем: убираем пробелы и хвостовую запятую
    TOKEN="$(printf '%s' "$TOKEN" | xargs 2>/dev/null || printf '%s' "$TOKEN")"
    ADMINS="$(printf '%s' "$ADMINS" | xargs 2>/dev/null || printf '%s' "$ADMINS")"
    PROXY="$(printf '%s' "$PROXY" | xargs 2>/dev/null || printf '%s' "$PROXY")"
    ADMINS="${ADMINS%,}"

    if [ -n "$TOKEN" ]; then
        sed -i -E "s#^  bot_token:.*#  bot_token: \"$TOKEN\"#" "$cfg"
        [ -n "$ADMINS" ] && sed -i -E "s#^  admin_ids:.*#  admin_ids: [$ADMINS]#" "$cfg"
        [ -n "$PROXY" ] && sed -i -E "s#^  proxy:.*#  proxy: \"$PROXY\"#" "$cfg"
        sed -i -E "s#^  disabled:.*#  disabled: false#" "$cfg"
        ok "Telegram bot configured"
    else
        # no token -> run without the bot so the service starts cleanly
        sed -i -E "s#^  disabled:.*#  disabled: true#" "$cfg"
        info "Telegram token not provided -> bot is disabled; edit $cfg later and set 'disabled: false'"
    fi
    ok "config -> $cfg"
}

install_assets() {
    cp "$INSTALL_DIR/deploy/proxygo.service" /etc/systemd/system/proxygo.service
    cp "$INSTALL_DIR/deploy/proxygo" /usr/local/bin/proxygo
    cp "$INSTALL_DIR/deploy/proxygo-build.sh" "$BIN/proxygo-build.sh"
    chmod +x /usr/local/bin/proxygo "$BIN/proxygo-build.sh"
    ok "systemd unit + CLI installed"

    if have_systemd; then
        systemctl daemon-reload >/dev/null 2>&1 || true
        systemctl enable proxygo >/dev/null 2>&1 || true
        systemctl restart proxygo >/dev/null 2>&1 || info "service start deferred (check 'proxygo status')"
    fi
}

summary() {
    cat <<EOF

${GRN}============================================================${R}
${B}  proxygo — установлен${R}
${GRN}============================================================${R}
  Установка      : ${B}$INSTALL_DIR${R}   (systemd: ${B}proxygo.service${R})
  Go-бинарь      : ${B}$BIN/proxygo${R}
  Java-агент     : ${B}$INSTALL_DIR/proxygo-mc-agent/target/proxygo-mc-agent.jar${R}
  Конфиг         : ${B}$INSTALL_DIR/config.yaml${R}
  Данные          : ${B}$INSTALL_DIR/data/${R}   Логи: ${B}$INSTALL_DIR/log/${R}
  Тулчейны        : ${B}$TOOL/{go,jdk,maven}${R}   (локально, не глобально)

${B}  Как пользоваться (единый CLI)${R}
${C_CYAN}  proxygo start    ${R}  запустить
${C_CYAN}  proxygo stop     ${R}  остановить (= systemctl stop proxygo)
${C_CYAN}  proxygo restart  ${R}  перезапустить
${C_CYAN}  proxygo status   ${R}  статус
${C_CYAN}  proxygo logs [N] ${R}  хвост лога
${C_CYAN}  proxygo backends ${R}  список бэкендов (БД)
${C_CYAN}  proxygo bans     ${R}  список банов (БД)
${C_CYAN}  proxygo stats    ${R}  статистика (БД)
${C_CYAN}  proxygo config   ${R}  показать конфиг
${C_CYAN}  proxygo build    ${R}  перекомпилировать (Go + Java)
${C_CYAN}  proxygo update   ${R}  обновить (git pull + build + restart)
${C_CYAN}  proxygo remove -y ${R} удалить proxygo
${DIM}  Полная справка: proxygo help${R}

${B}  Telegram${R}   настрой в ${B}$INSTALL_DIR/config.yaml${R} (bot_token, admin_ids, disabled).
${B}  Java-агент${R}  jar собран — скопируй его на серверы с Minecraft и добавь
                 в запуск: ${B}java -javaagent:proxygo-mc-agent.jar -jar server.jar nogui${R}

${GRN}============================================================${R}
EOF
}

# -------------------------------------------------------------------- run ---
need_root
detect_arch
ensure_basics
ensure_source
install_go
install_maven
install_jdk
build_sources
make_config
install_assets

[ "${warn_go:-0}" = "1" ]    && info "Go build skipped (toolchain missing)"
[ "${warn_agent:-0}" = "1" ] && info "Java agent build skipped (JDK/Maven missing)"

summary
log "готово."
