package proxy

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"

	"proxygo/internal/config"
	"proxygo/internal/logging"
	"proxygo/internal/metrics"
	"proxygo/internal/model"
	"proxygo/pkg/mcproto"
)

type stubStore struct{}

func (stubStore) IsBanned(string) (bool, error)    { return false, nil }
func (stubStore) RecordStat(model.StatPoint) error { return nil }

type stubNotify struct{}

func (stubNotify) Notify(string) {}

func testBackend(t *testing.T, mode string) *Backend {
	t.Helper()
	log, err := logging.New(config.Logging{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return &Backend{
		model: &model.Backend{Name: "t", BackendTCP: "127.0.0.1:25565", ForwardMode: mode},
		cfg:   &config.Config{},
		log:   log,
	}
}

func loginStartFrame(name string) []byte {
	body := []byte{byte(len(name))}
	body = append(body, name...)
	payload := append([]byte{0}, body...)
	raw := []byte{byte(len(payload))}
	return append(raw, payload...)
}

func TestForwardBungee(t *testing.T) {
	b := testBackend(t, model.ForwardBungee)
	cliA, cliB := net.Pipe()
	srvA, srvB := net.Pipe()
	defer cliA.Close()
	defer srvB.Close()

	raddr := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 54321}
	errCh := make(chan error, 1)
	go func() { errCh <- b.forwardBungee(raddr, cliB, srvA) }()

	hs := &mcproto.Handshake{Protocol: 760, Host: "mc.de", Port: 25565, Intent: 2}
	login := loginStartFrame("Steve")
	if _, err := cliA.Write(mcproto.MarshalHandshake(hs, hs.Host).Raw); err != nil {
		t.Fatal(err)
	}
	if _, err := cliA.Write(login); err != nil {
		t.Fatal(err)
	}

	_ = srvB.SetReadDeadline(time.Now().Add(3 * time.Second))
	f1, err := mcproto.ReadFrame(srvB)
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	got, err := mcproto.ParseHandshake(f1)
	if err != nil {
		t.Fatalf("parse rewritten handshake: %v", err)
	}
	want := "mc.de\x00203.0.113.7\x00" + mcproto.OfflineUUID("Steve")
	if got.Host != want {
		t.Fatalf("forwarded host = %q, want %q", got.Host, want)
	}

	f2, err := mcproto.ReadFrame(srvB)
	if err != nil {
		t.Fatalf("read login frame: %v", err)
	}
	if !bytes.Equal(f2.Raw, login) {
		t.Fatalf("login frame altered")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("forwardBungee: %v", err)
	}
}

func TestForwardBungeeStatusPingPassthrough(t *testing.T) {
	b := testBackend(t, model.ForwardBungee)
	cliA, cliB := net.Pipe()
	srvA, srvB := net.Pipe()
	defer cliA.Close()
	defer srvB.Close()

	raddr := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 54321}
	errCh := make(chan error, 1)
	go func() { errCh <- b.forwardBungee(raddr, cliB, srvA) }()

	// intent=1 (status ping) must be replayed verbatim, no login-start read.
	hs := &mcproto.Handshake{Protocol: 760, Host: "mc.de", Port: 25565, Intent: 1}
	raw := mcproto.MarshalHandshake(hs, hs.Host).Raw
	if _, err := cliA.Write(raw); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, len(raw))
	_ = srvB.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := readFull(srvB, buf); err != nil {
		t.Fatalf("read replay: %v", err)
	}
	if !bytes.Equal(buf, raw) {
		t.Fatalf("handshake not replayed verbatim")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("forwardBungee: %v", err)
	}
}

func TestWritePPv2(t *testing.T) {
	b := testBackend(t, model.ForwardPPv2)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- b.writePPv2(cli, srv) }()

	buf := make([]byte, 28)
	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := readFull(cli, buf); err != nil {
		t.Fatalf("read ppv2: %v", err)
	}
	sig := []byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}
	if !bytes.Equal(buf[:12], sig) {
		t.Fatalf("no ppv2 signature: %x", buf[:12])
	}
	// IPv4 header must carry the real client source address (127.0.0.1).
	if !strings.Contains(string(buf[16:28]), string([]byte{127, 0, 0, 1})) {
		t.Fatalf("client ip missing: %x", buf)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("writePPv2: %v", err)
	}
}

func readFull(c net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := c.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// TestBungeeEndToEnd runs a real Backend: client -> proxy listener -> fake
// backend, verifying the handshake arrives rewritten and bytes flow both ways.
func TestBungeeEndToEnd(t *testing.T) {
	// Fake MC backend: reads handshake + login frames, reports host, echoes a marker.
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upLn.Close()
	hostCh := make(chan string, 1)
	go func() {
		c, err := upLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		f1, err := mcproto.ReadFrame(c)
		if err != nil {
			hostCh <- "ERR:" + err.Error()
			return
		}
		hs, err := mcproto.ParseHandshake(f1)
		if err != nil {
			hostCh <- "ERR:" + err.Error()
			return
		}
		if _, err := mcproto.ReadFrame(c); err != nil {
			hostCh <- "ERR:login:" + err.Error()
			return
		}
		hostCh <- hs.Host
		// Read one extra byte (post-handshake client traffic) then echo a marker.
		var one [1]byte
		if _, err := readFull(c, one[:]); err != nil {
			return
		}
		_, _ = c.Write([]byte{0x42})
		time.Sleep(100 * time.Millisecond)
	}()

	log, err := logging.New(config.Logging{Level: "error"})
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	ms := metrics.New()
	defer ms.Stop()
	cfg := &config.Config{}
	cfg.Proxy.ListenInterface = "127.0.0.1"
	cfg.Proxy.DefaultIdle = time.Minute
	cfg.Proxy.BufferSize = 4096

	bk := newBackend(&model.Backend{
		Name: "e2e", BackendTCP: upLn.Addr().String(), ForwardMode: model.ForwardBungee,
	}, cfg, log, ms, stubStore{}, stubNotify{}, newIPConnTracker())
	if err := bk.start(); err != nil {
		t.Fatal(err)
	}
	defer bk.shutdown(0)

	cli, err := net.Dial("tcp", bk.tcpListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	_ = cli.SetDeadline(time.Now().Add(5 * time.Second))

	hs := &mcproto.Handshake{Protocol: 760, Host: "mc.de", Port: 25565, Intent: 2}
	if _, err := cli.Write(mcproto.MarshalHandshake(hs, hs.Host).Raw); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Write(loginStartFrame("Steve")); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Write([]byte{0x07}); err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if _, err := readFull(cli, marker[:]); err != nil {
		t.Fatalf("no echo marker: %v", err)
	}
	if marker[0] != 0x42 {
		t.Fatalf("bad marker %x", marker[0])
	}
	select {
	case got := <-hostCh:
		want := "mc.de\x00127.0.0.1\x00" + mcproto.OfflineUUID("Steve")
		if got != want {
			t.Fatalf("backend saw host %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend never got handshake")
	}
}
