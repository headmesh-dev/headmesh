package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7/dns"
	"github.com/cloudflare/cloudflare-go/v7/option"
)

const fixtureZoneID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestDNSCutoverPlanAndAmbiguousWrite(t *testing.T) {
	hCloud, cf, target := dnsCutoverFixture(t)
	cf.loseResponse = true
	client, cloudDNS := hCloud.client(t), cf.client(t)
	plan, err := PlanDNSCutover(t.Context(), client, cloudDNS, target)
	if err != nil || len(plan) != 5 || cf.writes() != 0 {
		t.Fatalf("preview = %v, err = %v, writes = %d", plan, err, cf.writes())
	}
	for _, change := range plan {
		if change.Name == cellHost {
			if change.Proxied || change.TTL != 60 || (change.Type == dnsA && change.PreviousTTL != 3600) {
				t.Fatalf("unexpected Headscale change: %+v", change)
			}
		} else if !change.Proxied || change.PreviousTTL != 1 || change.TTL != 1 || change.Type != dnsA {
			t.Fatalf("web record proxy and TTL must be preserved: %+v", change)
		}
	}
	hash := strings.Repeat("ab", 32)
	proofs := 0
	prove := func(_ context.Context, addresses StandbyIngress) error {
		proofs++
		if (addresses != StandbyIngress{IPv4: netip.MustParseAddr("192.0.2.91"), IPv6: netip.MustParseAddr("2001:db8:91::1"), NoiseKeySHA256: hash}) {
			t.Fatalf("proof not pinned to the Hetzner addresses and manifest: %+v", addresses)
		}
		return nil
	}
	applied, err := promoteDNS(t.Context(), client, cloudDNS, target, hash, prove)
	if err != nil || len(applied) != 5 || cf.writes() != 1 || proofs != 1 {
		t.Fatalf("promotion = %v, err = %v, writes = %d, proofs = %d", applied, err, cf.writes(), proofs)
	}
	applied, err = promoteDNS(t.Context(), client, cloudDNS, target, hash, prove)
	if err != nil || len(applied) != 0 || cf.writes() != 1 || proofs != 2 {
		t.Fatalf("repeat = %v, err = %v, writes = %d, proofs = %d", applied, err, cf.writes(), proofs)
	}
}

func TestDNSCutoverRejectsUnsafeState(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		setup      func(*standbyAPI, *cloudflareFixture)
		proof      func(*standbyAPI, *cloudflareFixture) error
		writes     int
	}{
		{name: "unfenced primary", want: "fence", setup: func(h *standbyAPI, _ *cloudflareFixture) { h.primary.status = "running" }},
		{name: "standby off", want: "running standby", setup: func(h *standbyAPI, _ *cloudflareFixture) { h.status = "off" }},
		{name: "unexpected DNS origin", want: "outside the fenced primary", setup: func(_ *standbyAPI, c *cloudflareFixture) { c.records[0].Content = "192.0.2.200" }},
		{name: "missed standby readiness", want: "ingress proof failed", proof: func(*standbyAPI, *cloudflareFixture) error { return errors.New("fixture readiness failed") }},
		{name: "lost fence during proof", want: "fence", proof: func(h *standbyAPI, _ *cloudflareFixture) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.primary.status = "running"
			return nil
		}},
		{name: "someone updated DNS during proof", want: "records changed", proof: func(_ *standbyAPI, c *cloudflareFixture) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.records[0].Content = "192.0.2.200"
			return nil
		}},
		{name: "provider did not apply the batch", want: "readback differs", writes: 1, setup: func(_ *standbyAPI, c *cloudflareFixture) { c.ignoreBatch = true }},
		{name: "provider discarded an existing proxy setting", want: "readback differs", writes: 1, setup: func(_ *standbyAPI, c *cloudflareFixture) { c.loseProxy = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hCloud, cf, target := dnsCutoverFixture(t)
			if tc.setup != nil {
				tc.setup(hCloud, cf)
			}
			_, err := promoteDNS(t.Context(), hCloud.client(t), cf.client(t), target,
				strings.Repeat("ab", 32), func(context.Context, StandbyIngress) error {
					if tc.proof != nil {
						return tc.proof(hCloud, cf)
					}
					return nil
				})
			if err == nil || !strings.Contains(err.Error(), tc.want) || cf.writes() != tc.writes {
				t.Fatalf("promotion err = %v, writes = %d, want %q and %d writes", err, cf.writes(), tc.want, tc.writes)
			}
		})
	}
}

func TestDNSCutoverRetainsAnExistingSiteAlias(t *testing.T) {
	hCloud, cf, target := dnsCutoverFixture(t)
	cf.records[3].Type = dnsCNAME
	cf.records[3].Content = "headmesh.dev."
	cf.pageSize = 1
	plan, err := PlanDNSCutover(t.Context(), hCloud.client(t), cf.client(t), target)
	if err != nil || len(plan) != 4 {
		t.Fatalf("preview = %v, err = %v", plan, err)
	}
	for _, change := range plan {
		if change.Name == "www.headmesh.dev" {
			t.Fatalf("the existing site alias should be retained: %+v", change)
		}
	}
}

type cfFixtureRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied *bool  `json:"proxied,omitempty"`
}

type cloudflareFixture struct {
	t                                    *testing.T
	mu                                   sync.Mutex
	records                              []cfFixtureRecord
	loseResponse, ignoreBatch, loseProxy bool
	batches                              int
	pageSize                             int
}

func dnsCutoverFixture(t *testing.T) (*standbyAPI, *cloudflareFixture, DNSCutover) {
	t.Helper()
	hCloud := newStandbyAPI(t)
	target := DNSCutover{Primary: fixturePrimary, Standby: fixtureStandbyTarget()}
	hCloud.name = standbyName(target.Primary.ServerID, target.Standby.RunID)
	hCloud.status = "running"
	return hCloud, &cloudflareFixture{t: t, records: []cfFixtureRecord{
		{ID: "api", Name: "api.headmesh.dev", Type: dnsA, Content: "192.0.2.71", TTL: 1, Proxied: new(true)},
		{ID: "apex", Name: siteHost, Type: dnsA, Content: "192.0.2.71", TTL: 1, Proxied: new(true)},
		{ID: "hs1", Name: cellHost, Type: dnsA, Content: "192.0.2.71", TTL: 3600, Proxied: new(false)},
		{ID: "www", Name: "www.headmesh.dev", Type: dnsA, Content: "192.0.2.71", TTL: 1, Proxied: new(true)},
		{ID: "txt", Name: siteHost, Type: "TXT", Content: "fixture verification record", TTL: 60},
	}}, target
}

func (c *cloudflareFixture) writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.batches
}

func (c *cloudflareFixture) client(t *testing.T) *CloudflareDNS {
	t.Helper()
	api := httptest.NewServer(c)
	t.Cleanup(api.Close)
	return &CloudflareDNS{zoneID: fixtureZoneID, records: dns.NewRecordService(option.WithBaseURL(api.URL+"/"),
		option.WithAPIToken("fixture"), option.WithMaxRetries(0))}
}

func (c *cloudflareFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer fixture" {
		c.t.Error("Cloudflare request omitted the scoped token")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	path := "/zones/" + fixtureZoneID + "/dns_records"
	switch {
	case r.URL.Path == path && r.Method == http.MethodGet:
		name := r.URL.Query().Get("name.exact")
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil || page < 1 {
			c.t.Errorf("invalid DNS inventory page: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		response := make([]cfFixtureRecord, 0)
		for _, record := range c.records {
			if record.Name == name {
				response = append(response, record)
			}
		}
		if len(response) == 0 {
			c.t.Errorf("unexpected DNS inventory query %q", name)
		}
		perPage := c.pageSize
		if perPage == 0 {
			perPage = 100
		}
		start := min((page-1)*perPage, len(response))
		end := min(start+perPage, len(response))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "result": response[start:end], "result_info": map[string]int{"page": page, "per_page": perPage},
		})
	case r.URL.Path == path+"/batch" && r.Method == http.MethodPost:
		c.batches++
		var batch struct {
			Patches []cfFixtureRecord
			Posts   []cfFixtureRecord
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&batch); err != nil {
			c.t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !c.ignoreBatch {
			for _, patch := range batch.Patches {
				if patch.Proxied != nil || patch.Content != "192.0.2.91" {
					c.t.Errorf("unexpected DNS patch: %+v", patch)
				}
				for i := range c.records {
					if c.records[i].ID == patch.ID {
						c.records[i].TTL, c.records[i].Content = patch.TTL, patch.Content
					}
				}
			}
			for _, post := range batch.Posts {
				if post.Name != cellHost || post.Type != dnsAAAA || post.Content != "2001:db8:91::1" ||
					post.TTL != 60 || post.Proxied == nil || *post.Proxied {
					c.t.Errorf("unexpected new DNS record: %+v", post)
				}
				post.ID = "hs1-aaaa"
				c.records = append(c.records, post)
			}
			if c.loseProxy {
				c.records[0].Proxied = new(false)
			}
		}
		if c.loseResponse {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
	default:
		c.t.Errorf("unexpected Cloudflare request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}
