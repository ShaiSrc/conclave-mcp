// Package fedrooms holds the storage side of federated rooms that the
// rooms, messages and federation packages share: stand-in keys for bots
// of other servers, the address of any key on the wire, and the event
// log of hosted rooms. It imports none of those packages, so each of
// them can import it.
package fedrooms

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
)

// OperatorMetaKey is the meta key holding the id of the operator key
// the web UI created.
const OperatorMetaKey = "ui_operator_key_id"

// addressRE is name@domain: the name in handle syntax (reserved words
// included, since hosts send operator@host and key-<id>@host), the
// domain in federation domain syntax with an optional port; StandIn
// also checks the domain with NormalizeDomain.
var addressRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}@[a-z0-9.-]{1,253}(:[0-9]{1,5})?$`)

// ErrAddress is StandIn's error for an address that cannot have a
// stand-in.
var ErrAddress = errors.New("fedrooms: no stand-in for this address")

// ErrInvalidDomain is returned by NormalizeDomain.
var ErrInvalidDomain = errors.New("invalid federation domain")

// NormalizeDomain returns the canonical form of a federation domain:
// lowercase ASCII letters, digits, dots and hyphens, an optional
// :port from 1 to 65535 without leading zeros, at most 253 characters
// in all. :443 is refused: it is the same server as no port, so it
// would be a second spelling. Labels are non-empty and neither start
// nor end with a hyphen. IDNA names are accepted only in their ASCII
// (xn--) form.
func NormalizeDomain(s string) (string, error) {
	if s == "" || len(s) > 253 {
		return "", ErrInvalidDomain
	}
	// ASCII only: strings.ToLower would fold some non-ASCII letters
	// (U+212A KELVIN SIGN) into ASCII ones, a homoglyph.
	s = strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
	host, port, hasPort := strings.Cut(s, ":")
	if hasPort {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || n == 443 || port != strconv.Itoa(n) {
			return "", ErrInvalidDomain
		}
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalidDomain
		}
		for _, c := range []byte(label) {
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
				return "", ErrInvalidDomain
			}
		}
	}
	return s, nil
}

// StandIn returns the id of the stand-in key for address, creating it
// if needed: label = address, remote_domain = its domain, trust origin
// external, empty hash and prefix. An invalid address (its domain in
// the canonical form of NormalizeDomain), or one that a local key holds
// as its label, is ErrAddress, never a stand-in.
func StandIn(ctx context.Context, tx *sql.Tx, address string) (int64, error) {
	domain := address[strings.IndexByte(address, '@')+1:]
	if d, err := NormalizeDomain(domain); !addressRE.MatchString(address) || err != nil || d != domain {
		return 0, fmt.Errorf("%w: invalid address %q", ErrAddress, address)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO api_keys (label, hash, prefix, created_at, trust_origin, remote_domain)
		 VALUES (?, '', '', ?, 'external', ?) ON CONFLICT(label) DO NOTHING`,
		address, storage.FormatTS(time.Now()), domain); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM api_keys WHERE label = ? AND remote_domain = ?`, address, domain).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: label %q belongs to a local key", ErrAddress, address)
	}
	return id, err
}

// DB is what Address needs; *sql.DB and *sql.Tx satisfy it.
type DB interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Address returns the address keyID has on the wire: a stand-in's
// label, handle@ourDomain for a bot, operator@ourDomain for the
// operator key, and key-<id>@ourDomain for any other local key.
func Address(ctx context.Context, db DB, keyID int64, ourDomain string) (string, error) {
	var (
		label, handle string
		remote        sql.NullString
		operator      bool
	)
	err := db.QueryRowContext(ctx,
		`SELECT k.label, k.remote_domain, COALESCE(k.handle, ''),
		        EXISTS (SELECT 1 FROM meta m WHERE m.key = ? AND m.value = CAST(k.id AS TEXT))
		 FROM api_keys k WHERE k.id = ?`, OperatorMetaKey, keyID,
	).Scan(&label, &remote, &handle, &operator)
	switch {
	case err != nil:
		return "", err
	case remote.Valid:
		return label, nil
	case handle != "":
		return handle + "@" + ourDomain, nil
	case operator:
		return "operator@" + ourDomain, nil
	}
	return "key-" + strconv.FormatInt(keyID, 10) + "@" + ourDomain, nil
}
