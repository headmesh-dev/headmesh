package policy

import (
	"fmt"
	"strconv"
	"strings"
)

// Port is one entry of a grant's "ip" list, for example "tcp:22", "udp:53", "tcp:8000-8100" or "*" (all
// protocols and ports; intra-org only).
type Port string

// PortAny means every protocol and port. It is only accepted for intra-org rules.
const PortAny Port = "*"

var knownProtocols = map[string]bool{"tcp": true, "udp": true, "sctp": true}

// ParsePort validates and normalises a port spec.
func ParsePort(s string) (Port, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "*" {
		return PortAny, nil
	}
	proto, rng, ok := strings.Cut(s, ":")
	if !ok {
		return "", fmt.Errorf("port %q: want proto:port, proto:lo-hi or *", s)
	}
	if !knownProtocols[proto] {
		return "", fmt.Errorf("port %q: unknown protocol %q", s, proto)
	}
	lo, hi, err := parseRange(rng)
	if err != nil {
		return "", fmt.Errorf("port %q: %w", s, err)
	}
	if lo == hi {
		return Port(fmt.Sprintf("%s:%d", proto, lo)), nil
	}
	return Port(fmt.Sprintf("%s:%d-%d", proto, lo, hi)), nil
}

func parseRange(s string) (lo, hi int, err error) {
	a, b, isRange := strings.Cut(s, "-")
	lo, err = parsePortNumber(a)
	if err != nil {
		return 0, 0, err
	}
	if !isRange {
		return lo, lo, nil
	}
	hi, err = parsePortNumber(b)
	if err != nil {
		return 0, 0, err
	}
	if hi < lo {
		return 0, 0, fmt.Errorf("range %d-%d is reversed", lo, hi)
	}
	return lo, hi, nil
}

func parsePortNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != s {
		return 0, fmt.Errorf("port number %q out of range 1-65535", s)
	}
	return n, nil
}

// portRange is the parsed form used for containment checks.
type portRange struct {
	proto  string // "" for PortAny
	lo, hi int
}

func (p Port) parsed() (portRange, error) {
	if p == PortAny {
		return portRange{proto: "", lo: 1, hi: 65535}, nil
	}
	norm, err := ParsePort(string(p))
	if err != nil {
		return portRange{}, err
	}
	proto, rng, _ := strings.Cut(string(norm), ":")
	lo, hi, err := parseRange(rng)
	if err != nil {
		return portRange{}, err
	}
	return portRange{proto: proto, lo: lo, hi: hi}, nil
}

// covers reports whether r allows every connection that o allows.
func (r portRange) covers(o portRange) bool {
	if r.proto != "" && r.proto != o.proto {
		return false
	}
	return r.lo <= o.lo && o.hi <= r.hi
}

// matches reports whether r allows proto/port.
func (r portRange) matches(proto string, port int) bool {
	return (r.proto == "" || r.proto == proto) && r.lo <= port && port <= r.hi
}

func checkPorts(ports []Port, allowAny bool) error {
	if len(ports) == 0 {
		return fmt.Errorf("at least one port is required")
	}
	for _, p := range ports {
		if p == PortAny {
			if !allowAny {
				return fmt.Errorf("\"*\" is not allowed here; list explicit ports")
			}
			continue
		}
		norm, err := ParsePort(string(p))
		if err != nil {
			return err
		}
		if norm != p {
			return fmt.Errorf("port %q is not normalised (want %q)", p, norm)
		}
	}
	return nil
}
