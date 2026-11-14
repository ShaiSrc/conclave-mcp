package federation

import (
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// protocolDoc is the protocol document whose Endpoints table lists the
// routes of the federation listener.
const protocolDoc = "../../docs/protocol.md"

// endpointsTable returns the first cell of each row of the first table
// under the "## Endpoints" heading of doc, backticks trimmed.
func endpointsTable(t *testing.T, doc string) []string {
	t.Helper()
	_, section, ok := strings.Cut(doc, "\n## Endpoints\n")
	if !ok {
		t.Fatal("no ## Endpoints section")
	}
	var rows []string
	inTable := false
	for line := range strings.SplitSeq(section, "\n") {
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		inTable = true
		cell, _, _ := strings.Cut(strings.TrimPrefix(line, "|"), "|")
		rows = append(rows, strings.Trim(strings.TrimSpace(cell), "`"))
	}
	if len(rows) < 3 {
		t.Fatalf("Endpoints table has %d rows", len(rows))
	}
	return rows[2:] // the header and its separator
}

// TestProtocolRoutes pins the federation listener's routes to the
// Endpoints table of the protocol document: a route added, removed or
// renamed fails it until both change.
func TestProtocolRoutes(t *testing.T) {
	r := chi.NewRouter()
	newLocal(t, "allowlist").PublicRoutes(r)
	var routed []string
	if err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routed = append(routed, method+" "+route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(protocolDoc)
	if err != nil {
		t.Fatal(err)
	}
	listed := endpointsTable(t, string(doc))
	slices.Sort(routed)
	if !slices.Equal(routed, slices.Sorted(slices.Values(listed))) { // a row listed twice differs too
		t.Errorf("routes and the Endpoints table of %s differ\nrouted:\n  %s\nlisted:\n  %s", protocolDoc,
			strings.Join(routed, "\n  "), strings.Join(listed, "\n  "))
	}
}
