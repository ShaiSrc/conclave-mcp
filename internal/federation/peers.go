package federation

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
)

// Peer statuses. blocked always wins; seen is an open-mode first contact.
const (
	statusAllowed = "allowed"
	statusBlocked = "blocked"
	statusSeen    = "seen"
)

// ErrPeerNotFound is returned for a domain without a peer row.
var ErrPeerNotFound = errors.New("federation peer not found")

// Peer is one row of federation_peers. PublicKey is the pinned key,
// nil for a domain blocked before it was seen. LastSeenAt is its last
// verified request, LastOutAt our last successful request to it.
type Peer struct {
	Domain, Status, Fingerprint, Note string
	PublicKey                         ed25519.PublicKey
	AddedAt                           time.Time
	LastSeenAt, LastOutAt             *time.Time
}

// Peers is the peer store: one row per domain, the status is the policy.
type Peers struct{ db *sql.DB }

const peerCols = `domain, status, fingerprint, public_key, note, added_at, last_seen_at, last_out_at`

func scanPeer(row interface{ Scan(...any) error }) (Peer, error) {
	var (
		p         Peer
		pub       []byte
		added     string
		seen, out sql.NullString
	)
	if err := row.Scan(&p.Domain, &p.Status, &p.Fingerprint, &pub, &p.Note, &added, &seen, &out); err != nil {
		return Peer{}, err
	}
	p.PublicKey = pub
	p.AddedAt, _ = time.Parse(time.RFC3339Nano, added)
	p.LastSeenAt, p.LastOutAt = parseTS(seen), parseTS(out)
	return p, nil
}

// parseTS is a nullable stored timestamp, nil for NULL.
func parseTS(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t, _ := time.Parse(time.RFC3339Nano, s.String)
	return &t
}

// Get returns the peer for domain, or ErrPeerNotFound.
func (ps *Peers) Get(ctx context.Context, domain string) (Peer, error) {
	p, err := scanPeer(ps.db.QueryRowContext(ctx, `SELECT `+peerCols+` FROM federation_peers WHERE domain = ?`, domain))
	if errors.Is(err, sql.ErrNoRows) {
		return Peer{}, ErrPeerNotFound
	}
	return p, err
}

// List returns all peers ordered by domain.
func (ps *Peers) List(ctx context.Context) ([]Peer, error) {
	rows, err := ps.db.QueryContext(ctx, `SELECT `+peerCols+` FROM federation_peers ORDER BY domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		p, err := scanPeer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Allow makes domain an allowed peer pinned to pub, replacing any
// earlier status, blocked included. A key it replaces goes to the
// peer's key history. For a domain that was blocked or had no row
// (removed, or never peered), its stand-ins, which a purge revoked, are
// no longer revoked, except those of bots the operator blocked.
func (ps *Peers) Allow(ctx context.Context, domain string, pub ed25519.PublicKey, note string) error {
	tx, err := ps.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var old []byte
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT public_key, status FROM federation_peers WHERE domain = ?`, domain).
		Scan(&old, &status); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(old) == ed25519.PublicKeySize && !pub.Equal(ed25519.PublicKey(old)) {
		if err := retireKey(ctx, tx, domain, old); err != nil {
			return err
		}
	}
	if status == "" || status == statusBlocked {
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = NULL WHERE remote_domain = ? AND revoked_at IS NOT NULL
			AND label NOT IN (SELECT address FROM blocked_bots)`, domain); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO federation_peers(domain, status, fingerprint, public_key, note, added_at)
		VALUES (?, 'allowed', ?, ?, ?, ?)
		ON CONFLICT(domain) DO UPDATE SET status = 'allowed', fingerprint = excluded.fingerprint,
			public_key = excluded.public_key, note = excluded.note`,
		domain, Fingerprint(pub), []byte(pub), note, storage.FormatTS(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

// Repin pins domain to pub in place of old, which goes to the peer's
// key history, keeping its status. It reports false, changing nothing,
// when the peer is no longer pinned to old.
func (ps *Peers) Repin(ctx context.Context, domain string, old, pub ed25519.PublicKey) (bool, error) {
	tx, err := ps.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE federation_peers SET fingerprint = ?, public_key = ? WHERE domain = ? AND public_key = ?`,
		Fingerprint(pub), []byte(pub), domain, []byte(old))
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if err := retireKey(ctx, tx, domain, old); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// retireKey puts pub, the key domain was pinned to until now, in the
// peer's key history. It was pinned when the key before it was retired,
// or when the peer was added.
func retireKey(ctx context.Context, tx *sql.Tx, domain string, pub ed25519.PublicKey) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO federation_peer_keys(domain, key_id, public_key, fingerprint, pinned_at, retired_at)
		SELECT ?, ?, ?, ?, MAX(added_at, COALESCE((SELECT MAX(retired_at) FROM federation_peer_keys WHERE domain = ?), '')), ?
		FROM federation_peers WHERE domain = ?
		ON CONFLICT(domain, key_id) DO UPDATE SET pinned_at = excluded.pinned_at, retired_at = excluded.retired_at`,
		domain, KeyIDFor(pub), []byte(pub), Fingerprint(pub), domain, storage.FormatTS(time.Now()), domain)
	return err
}

// History returns the keys in domain's key history: those it was
// pinned to before.
func (ps *Peers) History(ctx context.Context, domain string) ([]ed25519.PublicKey, error) {
	rows, err := ps.db.QueryContext(ctx, `SELECT public_key FROM federation_peer_keys WHERE domain = ?`, domain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ed25519.PublicKey
	for rows.Next() {
		var pub []byte
		if err := rows.Scan(&pub); err != nil {
			return nil, err
		}
		out = append(out, pub)
	}
	return out, rows.Err()
}

// Block makes domain a blocked peer, keeping a known key and note, and
// deletes its cached bots and its bots' pending and rejected follow
// requests.
func (ps *Peers) Block(ctx context.Context, domain string) error {
	_, err := ps.db.ExecContext(ctx, `INSERT INTO federation_peers(domain, status, added_at) VALUES (?, 'blocked', ?)
		ON CONFLICT(domain) DO UPDATE SET status = 'blocked'`,
		domain, storage.FormatTS(time.Now()))
	if err == nil {
		_, err = ps.db.ExecContext(ctx, `DELETE FROM follows WHERE status != 'accepted'
			AND follower_key_id IN (SELECT id FROM api_keys WHERE remote_domain = ?)`, domain)
	}
	if err != nil {
		return err
	}
	return ps.dropBots(ctx, domain)
}

// Remove deletes domain's row, its overrides and its cached bots, or
// returns ErrPeerNotFound.
func (ps *Peers) Remove(ctx context.Context, domain string) error {
	res, err := ps.db.ExecContext(ctx, `DELETE FROM federation_peers WHERE domain = ?`, domain)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrPeerNotFound
	}
	if _, err := ps.db.ExecContext(ctx, `DELETE FROM federation_peer_limits WHERE domain = ?`, domain); err != nil {
		return err
	}
	return ps.dropBots(ctx, domain)
}

// dropBots deletes domain's cached bots. It runs after the status
// change, so a refresh that checks the status in its transaction
// cannot bring them back.
func (ps *Peers) dropBots(ctx context.Context, domain string) error {
	_, err := ps.db.ExecContext(ctx, `DELETE FROM remote_bots WHERE domain = ?`, domain)
	return err
}

// RecordSeen records domain as seen with pub unless it has a row
// already, and reports whether it inserted one.
func (ps *Peers) RecordSeen(ctx context.Context, domain string, pub ed25519.PublicKey) (bool, error) {
	res, err := ps.db.ExecContext(ctx, `INSERT INTO federation_peers(domain, status, fingerprint, public_key, added_at)
		VALUES (?, 'seen', ?, ?, ?) ON CONFLICT(domain) DO NOTHING`,
		domain, Fingerprint(pub), []byte(pub), storage.FormatTS(time.Now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Touch sets domain's last_seen_at to now.
func (ps *Peers) Touch(ctx context.Context, domain string) error {
	_, err := ps.db.ExecContext(ctx, `UPDATE federation_peers SET last_seen_at = ? WHERE domain = ?`,
		storage.FormatTS(time.Now()), domain)
	return err
}

// TouchOut sets domain's last_out_at to now.
func (ps *Peers) TouchOut(ctx context.Context, domain string) error {
	_, err := ps.db.ExecContext(ctx, `UPDATE federation_peers SET last_out_at = ? WHERE domain = ?`,
		storage.FormatTS(time.Now()), domain)
	return err
}
