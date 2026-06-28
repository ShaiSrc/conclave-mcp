// Package mcp is the Model Context Protocol server entry point.
// It has no Go-level tests of its own: the surface area is
// covered by tests in tests/e2e and internal/mcp, and the CLI
// is exercised through cmd/conclave-mcp. This empty test file
// exists so `go test ./...` does not emit a "no test files"
// warning or fail with the covdata tool when -coverprofile
// is set.
package mcp
