package recovery

import (
	"encoding/binary"
	"hash/crc32"
	"net"
	"net/netip"
	"strings"
	"testing"
)

func TestSTUNBindingRoundTrip(t *testing.T) {
	for _, test := range []struct {
		name, network, listen, reflected, want string
	}{
		{"native IPv4", "udp4", "127.0.0.1", "192.0.2.99", ""},
		{"native IPv6", "udp6", "::1", "2001:db8::99", ""},
		{"IPv4 represented as mapped IPv6", "udp4", "127.0.0.1", "::ffff:192.0.2.99", ""},
		{"relay rewrites source to loopback", "udp4", "127.0.0.1", "127.0.0.1", "usable client address"},
		{"wrong address family", "udp6", "::1", "192.0.2.99", "usable client address"},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, err := net.ListenUDP(test.network, &net.UDPAddr{IP: net.ParseIP(test.listen)})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			done := make(chan struct{})
			go func() {
				defer close(done)
				var raw [64]byte
				n, sender, err := conn.ReadFromUDP(raw[:])
				if err != nil {
					t.Error(err)
					return
				}
				request := raw[:n]
				if n != 40 || string(request[24:32]) != "tailnode" || binary.BigEndian.Uint32(request[4:8]) != 0x2112a442 ||
					binary.BigEndian.Uint32(request[36:40]) != crc32.ChecksumIEEE(request[:32])^0x5354554e {
					t.Error("invalid Tailscale STUN request")
					return
				}
				if _, err := conn.WriteToUDP(stunFixtureResponse(request, netip.MustParseAddr(test.reflected)), sender); err != nil {
					t.Error(err)
				}
			}()
			err = probeSTUN(t.Context(), conn.LocalAddr().(*net.UDPAddr).AddrPort())
			_ = conn.Close()
			<-done
			if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("probe error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSTUNResponseRejectsDifferentTransactionAndTruncatedMapping(t *testing.T) {
	request, err := stunBindingRequest()
	if err != nil {
		t.Fatal(err)
	}
	response := stunFixtureResponse(request, netip.MustParseAddr("192.0.2.99"))
	response[8] ^= 1
	if err := checkSTUNResponse(request, response, true); err == nil {
		t.Fatal("accepted a response for another transaction")
	}
	response[8] ^= 1
	response = response[:len(response)-4]
	binary.BigEndian.PutUint16(response[2:4], 8)
	if err := checkSTUNResponse(request, response, true); err == nil {
		t.Fatal("accepted a truncated mapped address")
	}
}

func stunFixtureResponse(request []byte, mapped netip.Addr) []byte {
	ip := mapped.AsSlice()
	response := make([]byte, 28+len(ip))
	binary.BigEndian.PutUint16(response[0:2], 0x0101)
	binary.BigEndian.PutUint16(response[2:4], 8+uint16(len(ip)))
	copy(response[4:20], request[4:20])
	binary.BigEndian.PutUint16(response[20:22], 0x0020)
	binary.BigEndian.PutUint16(response[22:24], 4+uint16(len(ip)))
	if mapped.Is4() {
		response[25] = 1
	} else {
		response[25] = 2
	}
	binary.BigEndian.PutUint16(response[26:28], 51123^0x2112)
	for i, b := range ip {
		response[28+i] = b ^ request[4+i]
	}
	return response
}
