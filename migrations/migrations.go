// Package migrations holds the versioned SQL schema. The files are compiled into the
// binary, so the container image needs no extra files and the code always runs against
// the schema it was built with.
package migrations

import "embed"

// FS contains every *.sql file in this directory, named NNNNNN_title.{up,down}.sql as
// golang-migrate expects.
//
//go:embed *.sql
var FS embed.FS
