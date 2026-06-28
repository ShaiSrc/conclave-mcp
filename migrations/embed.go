// Package migrations embeds the .sql migration files so the binary
// can apply them without relying on the working directory at
// run time. The serve subcommand imports this package and passes
// the embedded fs.FS to storage.Migrate.
//
// Keeping the embed in its own package (rather than embedding
// directly in cmd/serve) avoids the //go:embed "all:.." limitation
// of going up the directory tree.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed *.sql
var files embed.FS

// FS returns the embedded migration filesystem rooted at "." so
// storage.Migrate can read 0001_initial.sql etc. directly.
func FS() fs.FS {
	return files
}
