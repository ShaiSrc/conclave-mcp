package storage

import "time"

// tsLayout is fixed width (always 9 fraction digits, always UTC "Z"),
// so string order in SQL equals time order. RFC3339Nano trims trailing
// zeros, which sorted ".1Z" after ".12Z" and made `since` cursors skip
// messages from the same second.
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

// FormatTS formats t for the ts columns compared in SQL
// (messages.ts, audit.ts).
func FormatTS(t time.Time) string { return t.UTC().Format(tsLayout) }

// NormalizeTS parses an RFC 3339 timestamp (any offset, any precision)
// into the FormatTS form, for `since` filters.
func NormalizeTS(s string) (string, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "", err
	}
	return FormatTS(t), nil
}
