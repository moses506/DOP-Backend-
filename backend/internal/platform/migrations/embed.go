// Package migrations embeds the platform's own SQL migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
