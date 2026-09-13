// Package ppv2 implements the HAProxy PROXY protocol, version 2.
//
// The version 2 header starts with a 12-byte signature followed by a
// version+command byte, a family+protocol byte and a 2-byte big-endian
// length. The header is used to carry the real client address across
// gliding proxies. Only the binary IPv4/IPv6 forms are supported here.
//
// Layout (RFC written as / spec from HAProxy docs):
//
//	0..11   : signature 0x0D 0x0A 0x0D 0x0A 0x00 0x0D 0x0A 0x51 0x55 0x49 0x54 0x0A
//	12      : version (0x2) << 4 | command (0x0 LOCAL | 0x1 PROXY)
//	13      : family << 4 | protocol (0x1 STREAM | 0x2 DGRAM)
//	14..15  : length (big-endian) of the following address block
//	16..    : address block
package ppv2

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// Signature is the 12-byte magic prefix of every PROXY v2 header.
var Signature = [12]byte{0x0D, 0x0A, 0x0D, 0x0A, 0x00, 0x0D, 0x0A, 0x51, 0x55, 0x49, 0x54, 0x0A}

// Command values (low nibble of byte 12).
const (
	CommandLocal = 0x0 // LOCAL: connection came from locally generated traffic
	CommandProxy = 0x1 // PROXY: transports the actual client address
)

// Family values (high nibble of byte 13).
const (
	FamilyUnspec = 0x0
	FamilyINET   = 0x1 // IPv4
	FamilyINET6  = 0x2 // IPv6
	FamilyUNIX   = 0x3
)

// Protocol values (low nibble of byte 13).
const (
	ProtocolUnspec = 0x0
	ProtocolStream = 0x1 // TCP
	ProtocolDGRAM  = 0x2 // UDP
)

// ErrNoHeader is returned by Parse when the stream does not start with the
// PROXY v2 signature, i.e. the connection is a direct link.
var ErrNoHeader = errors.New("ppv2: no PROXY signature")

// ErrInvalid is returned for a structurally invalid header.
var ErrInvalid = errors.New("ppv2: invalid header")

// Header is a parsed (or to-be-built) PROXY v2 header.
type Header struct {
	Command  byte
	Family   byte
	Protocol byte

	SourceAddr net.IP
	DestAddr   net.IP
	SourcePort uint16
	DestPort   uint16
}

// Version returns the header version (always 2 for this package).
func (h Header) Version() byte { return 0x2 }

// Size returns the total size of the encoded header in bytes.
func (h Header) Size() int { return 12 + 2 + h.addressBlockLen() }

// Valid reports whether the header is self-consistent and representable.
func (h Header) Valid() bool {
	if h.Command != CommandLocal && h.Command != CommandProxy {
		return false
	}
	switch h.Protocol {
	case ProtocolStream, ProtocolDGRAM:
	case ProtocolUnspec:
		// family must be unspecified too then
		if h.Family != FamilyUnspec {
			return false
		}
	default:
		return false
	}
	switch h.Family {
	case FamilyUnspec:
		return h.SourceAddr == nil && h.DestAddr == nil
	case FamilyINET:
		return h.SourceAddr != nil && h.DestAddr != nil &&
			h.SourceAddr.To4() != nil && h.DestAddr.To4() != nil
	case FamilyINET6:
		return h.SourceAddr != nil && h.DestAddr != nil &&
			h.SourceAddr.To4() == nil && h.DestAddr.To4() == nil
	case FamilyUNIX:
		// Not supported by the proxy (TCP/UDP to IP backends only).
		return false
	default:
		return false
	}
}

func (h Header) addressBlockLen() int {
	switch h.Family {
	case FamilyINET:
		return 4 + 4 + 2 + 2 // 12
	case FamilyINET6:
		return 16 + 16 + 2 + 2 // 36
	default:
		return 0
	}
}

// Marshal encodes the header into a freshly allocated buffer.
func (h Header) Marshal() ([]byte, error) {
	if !h.Valid() {
		return nil, fmt.Errorf("%w: malformed header %+v", ErrInvalid, h)
	}
	blockLen := h.addressBlockLen()
	buf := make([]byte, 16+blockLen)
	copy(buf[0:12], Signature[:])
	buf[12] = (0x2 << 4) | (h.Command & 0x0F)
	buf[13] = ((h.Family & 0x0F) << 4) | (h.Protocol & 0x0F)
	binary.BigEndian.PutUint16(buf[14:16], uint16(blockLen))

	off := 16
	switch h.Family {
	case FamilyINET:
		off = writeIP(buf, off, h.SourceAddr, 4)
		off = writeIP(buf, off, h.DestAddr, 4)
	case FamilyINET6:
		off = writeIP(buf, off, h.SourceAddr, 16)
		off = writeIP(buf, off, h.DestAddr, 16)
	}
	binary.BigEndian.PutUint16(buf[off:], h.SourcePort)
	binary.BigEndian.PutUint16(buf[off+2:], h.DestPort)
	return buf, nil
}

func writeIP(buf []byte, off int, ip net.IP, size int) int {
	if ip != nil {
		if size == 4 {
			copy(buf[off:], ip.To4())
		} else {
			copy(buf[off:], ip.To16())
		}
	}
	return off + size
}

// minHeaderSize is the fixed portion (signature + v/c + f/p + length).
const minHeaderSize = 16

// MaxHeaderSize is the largest header this parser will load (IPv6 DGRAM).
const MaxHeaderSize = 16 + 36

// ReadHeader fully reads and parses one PROXY v2 header from an io.Reader.
// It is incremental-safe: intermediate TCP segments that split the header
// are handled because we always read exactly the remaining bytes.
//
// If the first 12 bytes do not match the signature, ErrNoHeader is returned
// and nothing beyond the signature bytes is consumed.
func ReadHeader(r io.Reader) (Header, error) {
	sig := make([]byte, 12)
	if _, err := io.ReadFull(r, sig); err != nil {
		return Header{}, err
	}
	if !equal(sig, Signature[:]) {
		return Header{}, ErrNoHeader
	}
	rest := make([]byte, 4) // v/c, f/p, length (2 bytes)
	if _, err := io.ReadFull(r, rest); err != nil {
		return Header{}, err
	}
	cmd := rest[0] & 0x0F
	family := rest[1] >> 4
	proto := rest[1] & 0x0F
	blockLen := int(binary.BigEndian.Uint16(rest[2:4]))

	if blockLen > MaxHeaderSize-minHeaderSize {
		return Header{}, fmt.Errorf("%w: address block too large (%d)", ErrInvalid, blockLen)
	}
	block := make([]byte, blockLen)
	if _, err := io.ReadFull(r, block); err != nil {
		return Header{}, err
	}

	h := Header{Command: cmd, Family: family, Protocol: proto}
	if family == FamilyINET && blockLen >= 12 {
		h.SourceAddr = net.IPv4(block[0], block[1], block[2], block[3]).To4()
		h.DestAddr = net.IPv4(block[4], block[5], block[6], block[7]).To4()
		h.SourcePort = binary.BigEndian.Uint16(block[8:10])
		h.DestPort = binary.BigEndian.Uint16(block[10:12])
	} else if family == FamilyINET6 && blockLen >= 36 {
		h.SourceAddr = net.IP(block[0:16])
		h.DestAddr = net.IP(block[16:32])
		h.SourcePort = binary.BigEndian.Uint16(block[32:34])
		h.DestPort = binary.BigEndian.Uint16(block[34:36])
	}
	return h, nil
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
