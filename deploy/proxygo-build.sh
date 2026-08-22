#!/usr/bin/env bash
#
# proxygo-build.sh - rebuild the proxy binary and the Java agent in place using
# the project-local toolchains (Go, JDK, Maven) under /opt/proxygo/.tool.
# The toolchains are installed per-project and never touch the system.

set -euo pipefail

INSTALL_DIR="${PROXYGO_HOME:-/opt/proxygo}"
TOOL="$INSTALL_DIR/.tool"
GO="$TOOL/go/bin/go"
MAVEN="$TOOL/maven/bin/mvn"
JDK="$TOOL/jdk/bin/java"

log() { printf '\033[1;34m[proxygo-build]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[proxygo-build]\033[0m %s\n' "$*" >&2; exit 1; }

[ -d "$INSTALL_DIR" ] || die "no project at $INSTALL_DIR (run the installer first)"

# ---- Go proxy --------------------------------------------------------------
if [ -x "$GO" ]; then
    log "building Go proxy (local toolchain $TOOL/go)"
    export GOROOT="$TOOL/go"
    export PATH="$TOOL/go/bin:$PATH"
    export CGO_ENABLED=0
    ( cd "$INSTALL_DIR" && "$GO" build -mod=mod -trimpath -ldflags="-s -w" \
        -o bin/proxygo ./cmd/proxygo )
    log "binary -> $INSTALL_DIR/bin/proxygo"
else
    log "Go toolchain missing; skipping Go build (run installer)"
fi

# ---- Java agent ------------------------------------------------------------
if [ -x "$MAVEN" ] && [ -x "$JDK" ]; then
    log "building Java agent (local toolchain $TOOL/maven + $TOOL/jdk)"
    export JAVA_HOME="$TOOL/jdk"
    export PATH="$TOOL/jdk/bin:$TOOL/maven/bin:$PATH"
    ( cd "$INSTALL_DIR/proxygo-mc-agent" && "$MAVEN" -q -Dmaven.test.skip=true clean package )
    JAR="$INSTALL_DIR/proxygo-mc-agent/target/proxygo-mc-agent.jar"
    [ -f "$JAR" ] || { log "Java agent build did not produce $JAR"; exit 1; }
    log "agent jar -> $JAR"
else
    log "Maven/JDK missing; skipping Java agent (run installer)"
fi

log "done. rebuild via:   proxygo build"
