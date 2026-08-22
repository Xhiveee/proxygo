package ppv2

import (
	"bytes"
	"net"
	"testing"
)

func TestHeaderMarshalRoundTripIPv4(t *testing.T) {
	h := Header{
		Command:    CommandProxy,
		Family:     FamilyINET,
		Protocol:   ProtocolStream,
		SourceAddr: net.ParseIP("203.0.113.5").To4(),
		DestAddr:   net.ParseIP("193.23.221.21").To4(),
		SourcePort: 51234,
		DestPort:   25565,
	}
	if !h.Valid() {
		t.Fatalf("header should be valid: %+v", h)
	}
	buf, err := h.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(buf) != 16+12 {
		t.Fatalf("size = %d, want 28", len(buf))
	}
	if !bytes.Equal(buf[:12], Signature[:]) {
		t.Fatalf("bad signature: %x", buf[:12])
	}

	got, err := ReadHeader(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.SourceAddr.String() != "203.0.113.5" {
		t.Fatalf("src = %s", got.SourceAddr)
	}
	if got.DestAddr.String() != "193.23.221.21" {
		t.Fatalf("dst = %s", got.DestAddr)
	}
	if got.SourcePort != 51234 || got.DestPort != 25565 {
		t.Fatalf("ports = %d/%d", got.SourcePort, got.DestPort)
	}
	if got.Command != CommandProxy || got.Protocol != ProtocolStream || got.Family != FamilyINET {
		t.Fatalf("fields = %+v", got)
	}
}

func TestHeaderMarshalIPv6(t *testing.T) {
	h := Header{
		Command:    CommandProxy,
		Family:     FamilyINET6,
		Protocol:   ProtocolStream,
		SourceAddr: net.ParseIP("2001:db8::1"),
		DestAddr:   net.ParseIP("2001:db8::ff"),
		SourcePort: 1000,
		DestPort:   2000,
	}
	buf, err := h.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(buf) != 16+36 {
		t.Fatalf("size = %d, want %d", len(buf), 16+36)
	}
	got, err := ReadHeader(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.SourceAddr.String() != h.SourceAddr.String() {
		t.Fatalf("src = %s", got.SourceAddr)
	}
	if got.DestAddr.String() != h.DestAddr.String() {
		t.Fatalf("dst = %s", got.DestAddr)
	}
}

func TestReadHeaderNoSignature(t *testing.T) {
	// Minecraft starts with a VarInt protocol id; ensure at least a full
	// signature-length is present so we can conclude "no header".
	_, err := ReadHeader(bytes.NewReader([]byte{
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05,
		0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b,
	}))
	if err != ErrNoHeader {
		t.Fatalf("err = %v, want ErrNoHeader", err)
	}
}

func TestReadHeaderTruncated(t *testing.T) {
	// Only part of the signature.
	_, err := ReadHeader(bytes.NewReader([]byte{0x0D, 0x0A, 0x0D}))
	if err == nil {
		t.Fatal("expected error for truncated stream")
	}
}

func TestHeaderInvalid(t *testing.T) {
	cases := []Header{
		{Command: 7, Family: FamilyINET, Protocol: ProtocolStream},
		{Command: CommandProxy, Family: FamilyINET, Protocol: ProtocolDGRAM, SourceAddr: nil},
		{Command: CommandProxy, Family: FamilyUNIX, Protocol: ProtocolStream},
	}
	for i, h := range cases {
		if h.Valid() {
			t.Fatalf("case %d: %+v should be invalid", i, h)
		}
	}
}

func TestHeaderUDPDgramIPv4(t *testing.T) {
	h := Header{
		Command:    CommandProxy,
		Family:     FamilyINET,
		Protocol:   ProtocolDGRAM, // voice chat
		SourceAddr: net.ParseIP("198.51.100.7").To4(),
		DestAddr:   net.ParseIP("193.23.221.21").To4(),
		SourcePort: 40000,
		DestPort:   24454,
	}
	if !h.Valid() {
		t.Fatal("valid udp header marked invalid")
	}
	buf, err := h.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if buf[13]&0x0F != ProtocolDGRAM {
		t.Fatalf("proto nibble = %x", buf[13]&0x0F)
	}
}
