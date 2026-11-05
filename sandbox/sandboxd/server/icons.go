package server

import (
	"embed"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The Headmesh mark as the server icon clients show next to the sandbox
// (VS Code renders serverInfo.icons in its tool picker).
//
// VS Code accepts an HTTP server's icon only from the server's own authority,
// so the PNGs are served by sandboxd rather than linked from headmesh.dev.
// The mark is transparent with an ink body on light themes and a paper body
// on dark ones, so each theme gets its own file and the client picks.
//
//go:embed icons/light.png icons/dark.png
var iconFS embed.FS

var iconFiles = []struct {
	path  string
	theme mcp.IconTheme
}{
	{"icons/light.png", mcp.IconThemeLight},
	{"icons/dark.png", mcp.IconThemeDark},
}

// serverIcons advertises the embedded PNGs at the public URL clients connect to.
func serverIcons(publicURL string) []mcp.Icon {
	base := strings.TrimSuffix(publicURL, "/")
	if base == "" {
		return nil
	}
	icons := make([]mcp.Icon, 0, len(iconFiles))
	for _, f := range iconFiles {
		icons = append(icons, mcp.Icon{Source: base + "/" + f.path, MIMEType: "image/png", Sizes: []string{"512x512"}, Theme: f.theme})
	}
	return icons
}

// handleIcons serves the embedded PNGs. Unauthenticated: a client fetches
// them to draw its server list, before and apart from any MCP session.
func handleIcons(mux *http.ServeMux) {
	for _, f := range iconFiles {
		mux.HandleFunc("GET /"+f.path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeFileFS(w, r, iconFS, f.path)
		})
	}
}
