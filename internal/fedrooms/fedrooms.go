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
// domain in federation domain syntax with an optional port.
var addressRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}@[a-z0-9.-]{1,253}(:[0-9]{1,5})?$`)

// ErrAddress is StandIn's error for an address that cannot have a
// stand-in.
var ErrAddress = errors.New("fedrooms: no stand-in for this address")

// StandIn returns the id of the stand-in key for address, creating it
// if needed: label = address, remote_domain = its domain, trust origin
// external, empty hash and prefix. An invalid address, or one that a
// local key holds as its label, is ErrAddress, never a stand-in.
func StandIn(ctx context.Context, tx *sql.Tx, address string) (int64, error) {
	if !addressRE.MatchString(address) {
		return 0, fmt.Errorf("%w: invalid address %q", ErrAddress, address)
	}
	domain := address[strings.IndexByte(address, '@')+1:]
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
