package server

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// VS Code drops an HTTP server's icon unless it comes from the server's own
// authority, so the advertised URL and the route serving it are one contract.
func TestServerIconsAdvertisedAndServed(t *testing.T) {
	f := newFixture(t)
	base := strings.TrimSuffix(f.srv.cfg.Sandboxd.PublicURL, "/")
	cs := f.connect(t, f.fullToken, "2025-11-25")
	got := cs.InitializeResult().ServerInfo.Icons
	if len(got) != 2 {
		t.Fatalf("serverInfo.icons = %+v, want light and dark", got)
	}
	for i, theme := range []mcp.IconTheme{mcp.IconThemeLight, mcp.IconThemeDark} {
		want := base + "/icons/" + string(theme) + ".png"
		if got[i].Source != want || got[i].Theme != theme || got[i].MIMEType != "image/png" {
			t.Errorf("icon %d = %+v, want %s (%s)", i, got[i], want, theme)
		}

		// Unauthenticated, like a client drawing its server list.
		res, err := http.Get(f.http.URL + "/icons/" + string(theme) + ".png")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" {
			t.Errorf("GET %s icon: %d %q", theme, res.StatusCode, res.Header.Get("Content-Type"))
		}
		if !bytes.HasPrefix(body, []byte("\x89PNG\r\n\x1a\n")) {
			t.Errorf("GET %s icon: body is not a PNG", theme)
		}
	}
}

func TestServerIconsNeedAPublicURL(t *testing.T) {
	if icons := serverIcons(""); icons != nil {
		t.Errorf("serverIcons(\"\") = %+v, want none", icons)
	}
}
