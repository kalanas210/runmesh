// Package migrations carries the SQL schema as embedded files.
//
// It exists only so the .sql files can live at the repository root, where an
// operator expects to find them, while still being compiled into the binary:
// //go:embed cannot reach outside its own package directory, so the package
// goes to the files rather than the files to the package.
//
// Embedding rather than shipping a directory means a single binary is a
// complete deployment. There is no "did the migrations get copied into the
// image" failure mode, and the running code and the schema it expects cannot
// be different versions.
package migrations

import "embed"

// FS holds every migration, in filename order. The runner in internal/pgstore
// parses the leading integer as the version and refuses to start if the file
// for an already-applied version has changed.
//
//go:embed *.sql
var FS embed.FS
