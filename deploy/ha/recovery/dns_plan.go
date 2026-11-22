package recovery

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

const (
	dnsA     = "A"
	dnsAAAA  = "AAAA"
	dnsCNAME = "CNAME"
	cellHost = "hs1.headmesh.dev"
	siteHost = "headmesh.dev"
	cellTTL  = 60
)

var cutoverNames = [...]string{"api.headmesh.dev", siteHost, cellHost, "www.headmesh.dev"}

// DNSChange identifies one origin change. A missing RecordID creates a record.
type DNSChange struct {
	RecordID    string `json:"record_id,omitempty"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Previous    string `json:"previous,omitempty"`
	Content     string `json:"content"`
	Proxied     bool   `json:"proxied"`
	PreviousTTL int    `json:"previous_ttl,omitempty"`
	TTL         int    `json:"ttl"`
}

type dnsRecord struct {
	id, name, kind, content string
	proxied                 bool
	privateRouting          bool
	ttl                     int
}

type promotionAddresses struct {
	oldIPv4   netip.Addr
	oldIPv6   netip.Prefix // A primary without IPv6 has an invalid prefix.
	standbyV4 netip.Addr
	standbyV6 netip.Addr
}

func planDNS(current []dnsRecord, addresses promotionAddresses) ([]DNSChange, []dnsRecord, error) {
	var changes []DNSChange
	want := make([]dnsRecord, 0, len(current)+1)
	for _, name := range cutoverNames {
		byKind := make(map[string]dnsRecord, 3)
		for _, record := range current {
			if record.name != name {
				continue
			}
			if _, exists := byKind[record.kind]; exists || record.id == "" || record.ttl <= 0 || record.privateRouting {
				return nil, nil, fmt.Errorf("DNS records for %s have an ambiguous or unsupported origin", name)
			}
			byKind[record.kind] = record
		}
		if alias, exists := byKind[dnsCNAME]; exists {
			if len(byKind) != 1 || name == siteHost || name == cellHost || alias.content != siteHost {
				return nil, nil, fmt.Errorf("DNS alias for %s does not target the managed site", name)
			}
			want = append(want, alias)
			continue
		}
		if _, exists := byKind[dnsA]; !exists {
			return nil, nil, fmt.Errorf("required DNS A record for %s is missing", name)
		}
		for _, kind := range []string{dnsA, dnsAAAA} {
			record, exists := byKind[kind]
			if !exists {
				if name == cellHost && kind == dnsAAAA {
					changes = append(changes, DNSChange{Name: name, Type: kind, Content: addresses.standbyV6.String(), TTL: cellTTL})
					want = append(want, dnsRecord{name: name, kind: kind, content: addresses.standbyV6.String(), ttl: cellTTL})
				}
				continue
			}
			update, err := originChange(record, addresses)
			if err != nil {
				return nil, nil, err
			}
			if update.Previous != update.Content || update.PreviousTTL != update.TTL {
				changes = append(changes, update)
			}
			record.content, record.ttl = update.Content, update.TTL
			want = append(want, record)
		}
	}
	return changes, want, nil
}

func originChange(record dnsRecord, addresses promotionAddresses) (DNSChange, error) {
	origin, err := netip.ParseAddr(record.content)
	if err != nil {
		return DNSChange{}, fmt.Errorf("invalid %s origin for %s", record.kind, record.name)
	}
	destination, matchesPrimary := addresses.standbyV4, origin == addresses.oldIPv4
	if record.kind == dnsAAAA {
		destination, matchesPrimary = addresses.standbyV6, addresses.oldIPv6.Contains(origin)
	}
	if origin != destination && !matchesPrimary {
		return DNSChange{}, fmt.Errorf("%s record for %s points outside the fenced primary and this standby", record.kind, record.name)
	}
	ttl := record.ttl
	if record.name == cellHost {
		if record.proxied {
			return DNSChange{}, fmt.Errorf("the Headscale record must be DNS-only")
		}
		ttl = cellTTL
	}
	return DNSChange{
		RecordID: record.id, Name: record.name, Type: record.kind, Previous: origin.String(), Content: destination.String(),
		Proxied: record.proxied, PreviousTTL: record.ttl, TTL: ttl,
	}, nil
}

func matchesDNSRecords(actual, expected []dnsRecord) bool {
	if len(actual) != len(expected) {
		return false
	}
	actual = slices.Clone(actual)
	expected = slices.Clone(expected)
	compare := func(a, b dnsRecord) int { return cmp.Compare(a.name+"/"+a.kind, b.name+"/"+b.kind) }
	slices.SortFunc(actual, compare)
	slices.SortFunc(expected, compare)
	for i := range expected {
		if expected[i].id == "" {
			if actual[i].id == "" {
				return false
			}
			expected[i].id = actual[i].id
		}
		if actual[i] != expected[i] {
			return false
		}
	}
	return true
}

func canonicalDNSContent(kind, content string) string {
	if kind == dnsCNAME {
		return strings.ToLower(strings.TrimSuffix(content, "."))
	}
	if addr, err := netip.ParseAddr(content); err == nil {
		return addr.String()
	}
	return content
}
