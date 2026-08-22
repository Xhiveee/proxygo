package telegram

import (
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"proxygo/internal/proxy"
)

// shortUptime renders a duration as _d h:m.
func shortUptime(d time.Duration) string {
	d = d.Truncate(time.Second)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	hours := d / time.Hour
	d -= hours * time.Hour
	mins := d / time.Minute
	if days > 0 {
		return fmt.Sprintf("%dd%dh", days, hours)
	}
	return fmt.Sprintf("%dh%02dm", hours, mins)
}

// humanBytes renders a byte count in a human friendly unit.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}

// bkStatsTx returns the total TCP+UDP bytes of a backend.
func bkStatsTx(bk *proxy.Backend) int64 {
	in, out, udp, _, _ := bk.Stats()
	return in + out + udp
}

// findPortFrom extracts the port from a host:port string.
func findPortFrom(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(port)
	return n
}

// escapeHtml neutralises user-controlled text before HTML rendering.
func escapeHtml(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// tailFile returns the last n lines of path.
func tailFile(path string, n int) string {
	if n <= 0 {
		n = 1
	}
	f, err := os.Open(path)
	if err != nil {
		return "РЅРµ СѓРґР°Р»РѕСЃСЊ РѕС‚РєСЂС‹С‚СЊ Р»РѕРі: " + err.Error()
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "РЅРµ СѓРґР°Р»РѕСЃСЊ РїСЂРѕС‡РёС‚Р°С‚СЊ Р»РѕРі: " + err.Error()
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// enabledStr is a tiny helper for state display.
func enabledStr(e bool) string {
	if e {
		return "РІРєР»"
	}
	return "РІС‹РєР»"
}

var _ = sort.Slice
var _ = fmt.Sprintf
