#!/usr/bin/env bash
#
# proxygo-build.sh - rebuild the proxy binary in place using the project-local
# Go toolchain under /opt/proxygo/.tool.
# The toolchain is installed per-project and never touches the system.

set -euo pipefail

INSTALL_DIR="${PROXYGO_HOME:-/opt/proxygo}"
TOOL="$INSTALL_DIR/.tool"
GO="$TOOL/go/bin/go"

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

log "done. rebuild via:   proxygo build"
