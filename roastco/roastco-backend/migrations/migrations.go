// Package migrations embeds the schema so the CRM binary can migrate on
// boot regardless of working directory (Railway, Docker, local).
package migrations

import _ "embed"

//go:embed 001_init.sql
var Schema string
