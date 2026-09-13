// Package mcproto implements the minimal Minecraft protocol surface the
// proxy needs: reading length-prefixed frames, parsing the handshake and
// login-start packets, and rebuilding a handshake with a rewritten server
// address (BungeeCord-style IP forwarding).
//
// Frame layout (all packets):
//
//	length : varint           // size of packet-id + body
//	id     : varint
//	body   : []byte
//
// Handshake body (id 0x00, state=handshaking):
//
//	protocol : varint
//	host     : string (varint len + utf8)
//	port     : uint16 big-endian
//	intent   : varint (1 = status ping, 2 = login)
//
// Login start body (id 0x00, state=login):
//
//	name     : string16 (varint len + utf8)
package mcproto

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrame bounds one packet frame. The handshake and login-start packets
// are tiny (< ~1 KiB); this cap only guards against garbage lengths.
const MaxFrame = 1 << 16

// ErrNotHandshake is returned when the first frame is not a handshake.
var ErrNotHandshake = errors.New("mcproto: not a handshake frame")

// Frame is one raw packet as read from the wire.
type Frame struct {
	ID   int32  // packet id
	Body []byte // payload without the id varint
	Raw  []byte // full frame bytes incl. length prefix (for verbatim replay)
}

// ReadFrame reads one length-prefixed packet from r. Bytes are consumed
// exactly (no buffering beyond the frame), so r can be handed to a plain
// io.Copy-style pump afterwards.
func ReadFrame(r io.Reader) (*Frame, error) {
	var rawLen [5]byte
	var length int32
	n := 0
	for i := 0; i < 5; i++ {
		if _, err := io.ReadFull(r, rawLen[i:i+1]); err != nil {
			return nil, err
		}
		n++
		length |= int32(rawLen[i]&0x7F) << (7 * uint(i))
		if rawLen[i]&0x80 == 0 {
			break
		}
		if i == 4 {
			return nil, errors.New("mcproto: length varint too long")
		}
	}
	if length <= 0 || length > MaxFrame {
		return nil, fmt.Errorf("mcproto: bad frame length %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	id, idLen := decodeVarint(payload)
	raw := make([]byte, 0, n+int(length))
	raw = append(raw, rawLen[:n]...)
	raw = append(raw, payload...)
	return &Frame{ID: id, Body: payload[idLen:], Raw: raw}, nil
}

// Handshake is a parsed client handshake packet.
type Handshake struct {
	Protocol int32
	Host     string
	Port     uint16
	Intent   int32
}

// ParseHandshake decodes a handshake frame. Returns ErrNotHandshake when the
// packet id is not 0x00 or the body is malformed.
func ParseHandshake(f *Frame) (*Handshake, error) {
	if f.ID != 0 {
		return nil, ErrNotHandshake
	}
	r := bytes.NewReader(f.Body)
	proto, err := readVarint(r)
	if err != nil {
		return nil, ErrNotHandshake
	}
	host, err := readString(r)
	if err != nil {
		return nil, ErrNotHandshake
	}
	var portB [2]byte
	if _, err := io.ReadFull(r, portB[:]); err != nil {
		return nil, ErrNotHandshake
	}
	intent, err := readVarint(r)
	if err != nil {
		return nil, ErrNotHandshake
	}
	return &Handshake{
		Protocol: proto,
		Host:     host,
		Port:     binary.BigEndian.Uint16(portB[:]),
		Intent:   intent,
	}, nil
}

// MarshalHandshake rebuilds the handshake frame with a different host field.
// Everything else (protocol version, port, intent) is preserved verbatim.
func MarshalHandshake(h *Handshake, host string) *Frame {
	body := appendVarint(nil, h.Protocol)
	body = appendString(body, host)
	body = append(body, byte(h.Port>>8), byte(h.Port))
	body = appendVarint(body, h.Intent)

	payload := appendVarint(nil, 0)
	payload = append(payload, body...)
	raw := appendVarint(nil, int32(len(payload)))
	raw = append(raw, payload...)
	return &Frame{ID: 0, Body: body, Raw: raw}
}

// ParseLoginName extracts the username from a login-start frame.
func ParseLoginName(f *Frame) (string, error) {
	if f.ID != 0 {
		return "", errors.New("mcproto: not a login-start frame")
	}
	return readString(bytes.NewReader(f.Body))
}

// OfflineUUID computes the UUID an offline-mode server assigns to a player:
// Java's UUID.nameUUIDFromBytes("OfflinePlayer:" + name), i.e. a name-based
// MD5 UUID (version 3) rendered in the canonical dashed form.
func OfflineUUID(name string) string {
	sum := md5.Sum([]byte("OfflinePlayer:" + name))
	sum[6] = (sum[6] & 0x0f) | 0x30
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// ---------- varint / string codec -----------------------------------------

func decodeVarint(b []byte) (int32, int) {
	var v int32
	for i := 0; i < 5 && i < len(b); i++ {
		v |= int32(b[i]&0x7F) << (7 * uint(i))
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return 0, 0
}

func readVarint(r *bytes.Reader) (int32, error) {
	var v int32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= int32(b&0x7F) << (7 * uint(i))
		if b&0x80 == 0 {
			return v, nil
		}
	}
	return 0, errors.New("mcproto: varint too long")
}

func appendVarint(b []byte, v int32) []byte {
	u := uint32(v)
	for {
		bb := byte(u & 0x7F)
		u >>= 7
		if u == 0 {
			return append(b, bb)
		}
		b = append(b, bb|0x80)
	}
}

func readString(r *bytes.Reader) (string, error) {
	n, err := readVarint(r)
	if err != nil {
		return "", err
	}
	if n < 0 || int64(n) > int64(r.Len()) {
		return "", errors.New("mcproto: string overruns frame")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func appendString(b []byte, s string) []byte {
	b = appendVarint(b, int32(len(s)))
	return append(b, s...)
}
