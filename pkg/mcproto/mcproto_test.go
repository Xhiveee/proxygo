package mcproto

import (
	"bytes"
	"testing"
)

func TestHandshakeRoundTrip(t *testing.T) {
	orig := &Handshake{Protocol: 760, Host: "mc.example.com", Port: 25565, Intent: 2}
	f := MarshalHandshake(orig, orig.Host)

	got, err := ParseHandshake(f)
	if err != nil {
		t.Fatalf("ParseHandshake: %v", err)
	}
	if got.Protocol != 760 || got.Host != "mc.example.com" || got.Port != 25565 || got.Intent != 2 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestReadFrameParsesHandshake(t *testing.T) {
	orig := &Handshake{Protocol: 47, Host: "a.b", Port: 25565, Intent: 2}
	wire := MarshalHandshake(orig, orig.Host).Raw

	f, err := ReadFrame(bytes.NewReader(wire))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(f.Raw, wire) {
		t.Fatalf("raw mismatch")
	}
	hs, err := ParseHandshake(f)
	if err != nil {
		t.Fatalf("ParseHandshake: %v", err)
	}
	if hs.Host != "a.b" || hs.Intent != 2 {
		t.Fatalf("bad handshake: %+v", hs)
	}
}

func TestMarshalHandshakeRewritesHost(t *testing.T) {
	orig := &Handshake{Protocol: 760, Host: "mc.example.com", Port: 25565, Intent: 2}
	fwd := "mc.example.com\x00203.0.113.7\x00" + OfflineUUID("Steve")
	f := MarshalHandshake(orig, fwd)

	hs, err := ParseHandshake(f)
	if err != nil {
		t.Fatalf("ParseHandshake: %v", err)
	}
	if hs.Host != fwd {
		t.Fatalf("host not rewritten: %q", hs.Host)
	}
	if hs.Port != 25565 || hs.Protocol != 760 || hs.Intent != 2 {
		t.Fatalf("fields lost: %+v", hs)
	}
}

func TestParseLoginName(t *testing.T) {
	body := appendVarint(nil, int32(len("Steve")))
	body = append(body, "Steve"...)
	payload := appendVarint(nil, 0)
	payload = append(payload, body...)
	raw := appendVarint(nil, int32(len(payload)))
	raw = append(raw, payload...)

	f, err := ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	name, err := ParseLoginName(f)
	if err != nil {
		t.Fatalf("ParseLoginName: %v", err)
	}
	if name != "Steve" {
		t.Fatalf("name = %q", name)
	}
}

func TestOfflineUUIDFormat(t *testing.T) {
	u := OfflineUUID("Steve")
	if len(u) != 36 || u[8] != '-' || u[13] != '-' || u[18] != '-' || u[23] != '-' {
		t.Fatalf("bad uuid shape: %q", u)
	}
	if u[14] != '3' {
		t.Fatalf("not a v3 uuid: %q", u)
	}
	if c := u[19]; c != '8' && c != '9' && c != 'a' && c != 'b' {
		t.Fatalf("bad variant: %q", u)
	}
	if u != OfflineUUID("Steve") {
		t.Fatalf("non-deterministic")
	}
	if u == OfflineUUID("Alex") {
		t.Fatalf("collision")
	}
}

func TestReadFrameRejectsGarbage(t *testing.T) {
	// length varint claiming > MaxFrame
	raw := appendVarint(nil, MaxFrame+1)
	if _, err := ReadFrame(bytes.NewReader(raw)); err == nil {
		t.Fatalf("expected error")
	}
}
