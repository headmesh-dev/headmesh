package recovery

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"net"
	"net/netip"
	"time"
)

const (
	stunHeaderSize  = 20
	stunMagicCookie = 0x2112a442
)

func probeSTUN(ctx context.Context, target netip.AddrPort) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := new(net.Dialer).DialContext(ctx, "udp", target.String())
	if err != nil {
		return fmt.Errorf("connect to STUN listener: %w", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	request, err := stunBindingRequest()
	if err != nil {
		return err
	}
	if _, err := conn.Write(request); err != nil {
		return fmt.Errorf("send STUN binding request: %w", err)
	}
	var response [1024]byte
	n, err := conn.Read(response[:])
	if err != nil {
		return fmt.Errorf("receive STUN binding response: %w", err)
	}
	return checkSTUNResponse(request, response[:n], target.Addr().Is4())
}

// The pinned Headscale listener accepts Binding Requests with the Tailscale
// SOFTWARE attribute and a final FINGERPRINT. See RFC 8489 §14.
func stunBindingRequest() ([]byte, error) {
	request := make([]byte, stunHeaderSize+12+8)
	binary.BigEndian.PutUint16(request[0:2], 0x0001) // Binding Request
	binary.BigEndian.PutUint16(request[2:4], 20)     // Attribute bytes
	binary.BigEndian.PutUint32(request[4:8], stunMagicCookie)
	if _, err := rand.Read(request[8:20]); err != nil {
		return nil, fmt.Errorf("generate STUN transaction ID: %w", err)
	}
	binary.BigEndian.PutUint16(request[20:22], 0x8022) // SOFTWARE
	binary.BigEndian.PutUint16(request[22:24], 8)
	copy(request[24:32], "tailnode")
	binary.BigEndian.PutUint16(request[32:34], 0x8028) // FINGERPRINT
	binary.BigEndian.PutUint16(request[34:36], 4)
	binary.BigEndian.PutUint32(request[36:40], crc32.ChecksumIEEE(request[:32])^0x5354554e)
	return request, nil
}

func checkSTUNResponse(request, response []byte, ipv4 bool) error {
	if len(response) < stunHeaderSize || binary.BigEndian.Uint16(response[:2]) != 0x0101 ||
		!bytes.Equal(response[4:20], request[4:20]) || int(binary.BigEndian.Uint16(response[2:4])) != len(response)-stunHeaderSize {
		return fmt.Errorf("received an invalid STUN binding success response")
	}
	key := response[4:20] // Magic cookie followed by the transaction ID.
	for attrs := response[stunHeaderSize:]; len(attrs) > 0; {
		if len(attrs) < 4 {
			return fmt.Errorf("truncated STUN attribute header")
		}
		kind := binary.BigEndian.Uint16(attrs[:2])
		size := int(binary.BigEndian.Uint16(attrs[2:4]))
		padded := (size + 3) &^ 3
		if padded > len(attrs)-4 {
			return fmt.Errorf("truncated STUN attribute")
		}
		if kind == 0x0020 { // XOR-MAPPED-ADDRESS
			return checkSTUNMapping(attrs[4:4+size], key, ipv4)
		}
		attrs = attrs[4+padded:]
	}
	return fmt.Errorf("the STUN response omitted its mapped address")
}

func checkSTUNMapping(value, key []byte, ipv4 bool) error {
	switch {
	case len(value) == 8 && value[1] == 1:
	case len(value) == 20 && value[1] == 2:
	default:
		return fmt.Errorf("invalid STUN mapped address attribute")
	}
	var raw [16]byte
	for i, v := range value[4:] {
		raw[i] = v ^ key[i]
	}
	ip, _ := netip.AddrFromSlice(raw[:len(value)-4])
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.Is4() != ipv4 || binary.BigEndian.Uint16(value[2:4])^uint16(stunMagicCookie>>16) == 0 {
		return fmt.Errorf("the STUN response did not reflect a usable client address and port")
	}
	return nil
}
