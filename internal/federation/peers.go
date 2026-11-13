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
// nil for a domain blocked before it was seen.
type Peer struct {
	Domain, Status, Fingerprint, Note string
	PublicKey                         ed25519.PublicKey
	AddedAt                           time.Time
	LastSeenAt                        *time.Time
}

// Peers is the peer store: one row per domain, the status is the policy.
type Peers struct{ db *sql.DB }

const peerCols = `domain, status, fingerprint, public_key, note, added_at, last_seen_at`

func scanPeer(row interface{ Scan(...any) error }) (Peer, error) {
	var (
		p     Peer
		pub   []byte
		added string
		seen  sql.NullString
	)
	if err := row.Scan(&p.Domain, &p.Status, &p.Fingerprint, &pub, &p.Note, &added, &seen); err != nil {
		return Peer{}, err
	}
	p.PublicKey = pub
	p.AddedAt, _ = time.Parse(time.RFC3339Nano, added)
	if seen.Valid {
		t, _ := time.Parse(time.RFC3339Nano, seen.String)
		p.LastSeenAt = &t
	}
	return p, nil
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
// earlier status, blocked included.
func (ps *Peers) Allow(ctx context.Context, domain string, pub ed25519.PublicKey, note string) error {
	_, err := ps.db.ExecContext(ctx, `INSERT INTO federation_peers(domain, status, fingerprint, public_key, note, added_at)
		VALUES (?, 'allowed', ?, ?, ?, ?)
		ON CONFLICT(domain) DO UPDATE SET status = 'allowed', fingerprint = excluded.fingerprint,
			public_key = excluded.public_key, note = excluded.note`,
		domain, Fingerprint(pub), []byte(pub), note, storage.FormatTS(time.Now()))
	return err
}

// Block makes domain a blocked peer, keeping a known key and note.
func (ps *Peers) Block(ctx context.Context, domain string) error {
	_, err := ps.db.ExecContext(ctx, `INSERT INTO federation_peers(domain, status, added_at) VALUES (?, 'blocked', ?)
		ON CONFLICT(domain) DO UPDATE SET status = 'blocked'`,
		domain, storage.FormatTS(time.Now()))
	return err
}

// Remove deletes domain's row, or returns ErrPeerNotFound.
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
	return nil
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
