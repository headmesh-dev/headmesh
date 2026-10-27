// Package db embeds the SQL migrations so every binary carries the schema it expects.
package db

import "embed"

// Migrations holds the goose migrations, applied in filename order.
//
//go:embed migrations/*.sql
var Migrations embed.FS
