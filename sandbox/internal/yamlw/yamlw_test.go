package yamlw

import "testing"

func TestMarshal(t *testing.T) {
	doc := Map{
		{"services", Map{
			{"sandbox", Map{
				{"hostname", "asus-sandbox"},
				{"cpus", 6.5},
				{"environment", Map{{"HEADMESH_MODE", "regular"}, {"EMPTY", ""}}},
				{"devices", []string{"nvidia.com/gpu=all"}},
				{"volumes", List{
					Map{
						{"type", "bind"},
						{"source", "/a b"},
						{"target", "/a b"},
						{"read_only", true},
						{"bind", Map{{"recursive", "readonly"}, {"propagation", "rprivate"}}},
					},
					Map{{"type", "tmpfs"}, {"target", "/x"}, {"tmpfs", Map{{"size", 4096}, {"mode", 448}}}},
				}},
				{"empty", List{}},
				{"none", Map{}},
				{"weird key!", "v\"q\n"},
			}},
		}},
	}
	want := `services:
  sandbox:
    hostname: "asus-sandbox"
    cpus: 6.5
    environment:
      HEADMESH_MODE: "regular"
      EMPTY: ""
    devices:
      - "nvidia.com/gpu=all"
    volumes:
      - type: "bind"
        source: "/a b"
        target: "/a b"
        read_only: true
        bind:
          recursive: "readonly"
          propagation: "rprivate"
      - type: "tmpfs"
        target: "/x"
        tmpfs:
          size: 4096
          mode: 448
    empty: []
    none: {}
    "weird key!": "v\"q\n"
`
	if got := string(Marshal(doc)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
