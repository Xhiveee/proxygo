package proxy

import (
	"net"
	"testing"
	"time"

	"proxygo/internal/config"
	"proxygo/internal/logging"
	"proxygo/internal/metrics"
	"proxygo/internal/model"
)

// fakeNotifier records alerts.
type fakeNotifier struct{ msgs []string }

func (f *fakeNotifier) Notify(s string) { f.msgs = append(f.msgs, s) }

// fakeDataDir is a temporary directory for access logs.
func testLogger(t *testing.T) *logging.Logger {
	t.Helper()
	cfg := config.Default().Logging
	cfg.Format = "console"
	l, err := logging.New(cfg)
	if err != nil {
		t.Fatalf("logging: %v", err)
	}
	return l
}

func TestIPConnTracker(t *testing.T) {
	tr := newIPConnTracker()
	tr.inc("1.1.1.1")
	tr.inc("1.1.1.1")
	tr.inc("2.2.2.2")
	if tr.count("1.1.1.1") != 2 {
		t.Fatalf("count 1.1.1.1 = %d", tr.count("1.1.1.1"))
	}
	tr.dec("1.1.1.1")
	if tr.count("1.1.1.1") != 1 {
		t.Fatalf("after dec count = %d", tr.count("1.1.1.1"))
	}
	tr.dec("1.1.1.1")
	if tr.count("1.1.1.1") != 0 {
		t.Fatalf("count after removal = %d", tr.count("1.1.1.1"))
	}
}

// TestUDPForwarderEndToEnd binds a backend UDP echo socket, a forwarder and a
// client, then verifies a datagram survives the round trip with a fresh
// session.
func TestUDPForwarderEndToEnd(t *testing.T) {
	cfg := config.Default()
	cfg.Proxy.ListenInterface = "127.0.0.1"
	cfg.Proxy.UDPSession = 60 * time.Second
	cfg.Proxy.DefaultIdle = 30 * time.Minute

	// Fake backend echo server.
	backendConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	defer backendConn.Close()
	backendAddr := backendConn.LocalAddr().String()

	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := backendConn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = backendConn.WriteTo(buf[:n], from)
		}
	}()

	log := testLogger(t)
	notif := &fakeNotifier{}
	ms := metrics.New()
	defer ms.Stop()

	rec := &model.Backend{
		Name:       "test",
		BackendTCP: "127.0.0.1:25565",
		UDPEnabled: true,
		UDPPort:    freeUDPPort(t),
		BackendUDP: backendAddr,
	}
	fwd, err := newUDPForwarder(rec, cfg, log, ms, notif)
	if err != nil {
		t.Fatalf("newUDPForwarder: %v", err)
	}
	if err := fwd.Start(); err != nil {
		t.Fatalf("fwd start: %v", err)
	}
	defer fwd.Close()

	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}
	defer client.Close()

	payload := []byte("voice-chat-packet")
	if _, err := client.WriteTo(payload, fwd.conn.LocalAddr()); err != nil {
		t.Fatalf("client write: %v", err)
	}

	reply := make([]byte, 1500)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := client.ReadFrom(reply)
	if err != nil {
		t.Fatalf("no reply to client: %v", err)
	}
	if string(reply[:n]) != string(payload) {
		t.Fatalf("reply mismatch: %q", reply[:n])
	}
	if fwd.packets.Load() != 1 {
		t.Fatalf("packets = %d", fwd.packets.Load())
	}
}

// freeUDPPort returns an available UDP port on loopback.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()
	return port
}
