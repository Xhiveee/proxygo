# syntax=docker/dockerfile:1
# --- build stage -----------------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src

# Cache module downloads. go.sum is generated on the first build (go mod tidy).
COPY go.mod ./
COPY . .
RUN go build -mod=mod -trimpath -ldflags="-s -w" \
    -o /out/proxygo ./cmd/proxygo

# --- runtime stage ---------------------------------------------------------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata iptables \
    && addgroup -S proxygo && adduser -S proxygo -G proxygo

WORKDIR /opt/proxygo
COPY --from=build /out/proxygo /usr/local/bin/proxygo
COPY config.example.yaml /opt/proxygo/config.yaml

# Runtime dirs (bind-mount in production).
RUN mkdir -p /opt/proxygo/data /opt/proxygo/log/access \
    && chown -R proxygo:proxygo /opt/proxygo

USER proxygo
EXPOSE 25565 24454

# If you mirror bans to iptables you must run with --cap-add=NET_ADMIN.
ENTRYPOINT ["/usr/local/bin/proxygo", "-config", "/opt/proxygo/config.yaml"]
