package recovery

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/dns"
	"github.com/cloudflare/cloudflare-go/v7/option"
)

// CloudflareDNS is scoped to a pinned recovery zone and its DNS records.
type CloudflareDNS struct {
	zoneID  string
	records *dns.RecordService
}

var zoneIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func NewCloudflareDNS(zoneID, token string) (*CloudflareDNS, error) {
	if !zoneIDPattern.MatchString(zoneID) || strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("recovery requires a Cloudflare zone ID and scoped DNS API token")
	}
	httpClient := &http.Client{
		Timeout: 12 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &CloudflareDNS{
		zoneID: zoneID,
		records: dns.NewRecordService(option.WithBaseURL("https://api.cloudflare.com/client/v4/"),
			option.WithAPIToken(token), option.WithHTTPClient(httpClient), option.WithMaxRetries(0)),
	}, nil
}

func (c *CloudflareDNS) readManagedRecords(ctx context.Context) ([]dnsRecord, error) {
	var records []dnsRecord
	for _, name := range cutoverNames {
		current, err := c.readRecordsForName(ctx, name)
		if err != nil {
			return nil, err
		}
		records = append(records, current...)
	}
	return records, nil
}

func (c *CloudflareDNS) readRecordsForName(ctx context.Context, name string) ([]dnsRecord, error) {
	var records []dnsRecord
	seen := make(map[string]bool)
	for page := 1; ; page++ {
		result, err := c.records.List(ctx, dns.RecordListParams{
			ZoneID: cloudflare.F(c.zoneID), Name: cloudflare.F(dns.RecordListParamsName{Exact: cloudflare.F(name)}),
			Page: cloudflare.F(float64(page)), PerPage: cloudflare.F(100.0),
		})
		if err != nil {
			return nil, fmt.Errorf("list Cloudflare DNS records for %s: %w", name, err)
		}
		if result == nil {
			return nil, fmt.Errorf("no Cloudflare DNS inventory returned for %s", name)
		}
		for _, record := range result.Result {
			if record.Name != name {
				return nil, fmt.Errorf("unexpected record in the Cloudflare DNS filter for %s", name)
			}
			if record.ID == "" || seen[record.ID] {
				return nil, fmt.Errorf("empty or repeated Cloudflare DNS record ID for %s", name)
			}
			seen[record.ID] = true
			kind := string(record.Type)
			switch kind {
			case dnsA, dnsAAAA, dnsCNAME:
				records = append(records, dnsRecord{
					id: record.ID, name: name, kind: kind, content: canonicalDNSContent(kind, record.Content),
					proxied: record.Proxied, privateRouting: record.PrivateRouting, ttl: int(record.TTL),
				})
			case "HTTPS", "SVCB":
				return nil, fmt.Errorf("%s has a %s record that needs explicit origin review", name, kind)
			}
		}
		if len(result.Result) == 0 || int64(len(result.Result)) < result.ResultInfo.PerPage {
			return records, nil
		}
	}
}

func (c *CloudflareDNS) apply(ctx context.Context, changes []DNSChange) error {
	var patches []dns.BatchPatchUnionParam
	var creates []dns.RecordBatchParamsPostUnion
	for _, change := range changes {
		if change.Type == dnsA {
			patches = append(patches, dns.BatchPatchARecordParam{
				ID: cloudflare.F(change.RecordID),
				ARecordParam: dns.ARecordParam{
					Name: cloudflare.F(change.Name), Type: cloudflare.F(dns.ARecordTypeA),
					Content: cloudflare.F(change.Content), TTL: cloudflare.F(dns.TTL(change.TTL)),
				},
			})
			continue
		}
		ipv6 := dns.AAAARecordParam{
			Name: cloudflare.F(change.Name), Type: cloudflare.F(dns.AAAARecordTypeAAAA),
			Content: cloudflare.F(change.Content), TTL: cloudflare.F(dns.TTL(change.TTL)),
		}
		if change.RecordID == "" {
			ipv6.Proxied = cloudflare.F(false)
			creates = append(creates, ipv6)
		} else {
			patches = append(patches, dns.BatchPatchAAAARecordParam{ID: cloudflare.F(change.RecordID), AAAARecordParam: ipv6})
		}
	}
	params := dns.RecordBatchParams{ZoneID: cloudflare.F(c.zoneID)}
	if len(patches) != 0 {
		params.Patches = cloudflare.F(patches)
	}
	if len(creates) != 0 {
		params.Posts = cloudflare.F(creates)
	}
	if _, err := c.records.Batch(ctx, params); err != nil {
		return fmt.Errorf("submit Cloudflare DNS cutover batch: %w", err)
	}
	return nil
}
