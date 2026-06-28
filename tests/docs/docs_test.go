// Doc line-count guard (SPEC-0007 §6.4, Vikunja #38).
//
// Asserts that README.md and docs/runbook.md are each <= 200
// lines (T7.5 in SPEC-0007 §4). The check is in a test so CI
// fails loudly when docs drift above the limit.
package docs

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const maxLines = 200

func TestDocs_LineCount(t *testing.T) {
	// Locate the repo root from this test file's location.
	// This file lives at <repo>/tests/docs/docs_test.go, so
	// the repo root is two directories up.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))

	cases := []struct {
		path string
		name string
	}{
		{filepath.Join(repoRoot, "README.md"), "README.md"},
		{filepath.Join(repoRoot, "docs", "runbook.md"), "docs/runbook.md"},
	}
	for _, c := range cases {
		n, err := countLines(c.path)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if n > maxLines {
			t.Errorf("%s: %d lines, want <= %d (SPEC-0007 §6.4)", c.name, n, maxLines)
		}
	}
}

// countLines returns the line count of a file, measured by
// bufio.Scanner (so the trailing newline is not counted as a
// separate empty line).
func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	n := 0
	for scanner.Scan() {
		n++
	}
	return n, scanner.Err()
}
