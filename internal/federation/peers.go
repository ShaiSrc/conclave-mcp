package federation

import (
	"context"
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

// Peer is one row of federation_peers.
type Peer struct {
	Domain, Status, Fingerprint, Note string
	AddedAt                           time.Time
	LastSeenAt                        *time.Time
}

// Peers is the peer store: one row per domain, the status is the policy.
type Peers struct{ db *sql.DB }

const peerCols = `domain, status, fingerprint, note, added_at, last_seen_at`

func scanPeer(row interface{ Scan(...any) error }) (Peer, error) {
	var (
		p     Peer
		added string
		seen  sql.NullString
	)
	if err := row.Scan(&p.Domain, &p.Status, &p.Fingerprint, &p.Note, &added, &seen); err != nil {
		return Peer{}, err
	}
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

// Allow makes domain an allowed peer pinned to fingerprint, replacing
// any earlier status, blocked included.
func (ps *Peers) Allow(ctx context.Context, domain, fingerprint, note string) error {
	_, err := ps.db.ExecContext(ctx, `INSERT INTO federation_peers(domain, status, fingerprint, note, added_at)
		VALUES (?, 'allowed', ?, ?, ?)
		ON CONFLICT(domain) DO UPDATE SET status = 'allowed', fingerprint = excluded.fingerprint, note = excluded.note`,
		domain, fingerprint, note, storage.FormatTS(time.Now()))
	return err
}

// Block makes domain a blocked peer, keeping a known fingerprint and note.
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

// RecordSeen records domain as seen with fingerprint unless it has a
// row already, and reports whether it inserted one.
func (ps *Peers) RecordSeen(ctx context.Context, domain, fingerprint string) (bool, error) {
	res, err := ps.db.ExecContext(ctx, `INSERT INTO federation_peers(domain, status, fingerprint, added_at)
		VALUES (?, 'seen', ?, ?) ON CONFLICT(domain) DO NOTHING`,
		domain, fingerprint, storage.FormatTS(time.Now()))
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
