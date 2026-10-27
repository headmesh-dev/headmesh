// Package compose embeds the base Compose file so the host CLI is
// self-contained: `headmesh-sandbox render` writes it next to the generated
// override in ${root}/compose.
package compose

import _ "embed"

// File is compose.yaml.
//
//go:embed compose.yaml
var File []byte

// EnvExample is .env.example.
//
//go:embed .env.example
var EnvExample []byte
