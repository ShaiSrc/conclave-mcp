// Package migrations embeds the SQL files that the storage
// layer applies at startup. It has no Go-level tests of its
// own: the migrations are exercised end-to-end by the e2e
// tests in tests/e2e and the per-package tests in
// internal/storage. This empty test file exists so
// `go test ./...` does not emit a "no test files" warning
// or fail with the covdata tool when -coverprofile is set.
package migrations
