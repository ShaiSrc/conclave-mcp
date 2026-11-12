package ui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
)

// newToken returns 32 random bytes as base64url and the form stored in
// the database (hashToken).
func newToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

// hashToken is the stored form of a token: hex SHA-256. Tokens are 256
// random bits, so a plain hash is enough (no salt, no argon2id).
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// store reads and writes the ui_* tables (migration 0009).
type store struct {
	db  *sql.DB
	now func() time.Time // tests move the clock
}

// createSession starts a session and returns its token (the cookie
// value). It also purges what has expired.
func (s *store) createSession(ctx context.Context) (string, error) {
	if err := s.purge(ctx); err != nil {
		return "", err
	}
	token, hash, err := newToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO ui_sessions (token_hash, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?)`,
		hash, storage.FormatTS(now), storage.FormatTS(now), storage.FormatTS(now.Add(MaxSession)))
	return token, err
}

// session reports whether token belongs to a live session (younger than
// MaxSession, used within IdleTimeout) and, if so, marks it used now.
func (s *store) session(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE ui_sessions SET last_seen_at = ?
		 WHERE token_hash = ? AND expires_at > ? AND last_seen_at > ?`,
		storage.FormatTS(now), hashToken(token), storage.FormatTS(now), storage.FormatTS(now.Add(-IdleTimeout)))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// deleteSession ends the session of token (logout).
func (s *store) deleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// deleteOtherSessions ends every session except the one of token.
func (s *store) deleteOtherSessions(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ui_sessions WHERE token_hash != ?`, hashToken(token))
	return err
}

// purge deletes expired sessions and setup tokens.
func (s *store) purge(ctx context.Context) error {
	now := s.now()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM ui_sessions WHERE expires_at <= ? OR last_seen_at <= ?`,
		storage.FormatTS(now), storage.FormatTS(now.Add(-IdleTimeout))); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM ui_setup_tokens WHERE expires_at <= ?`, storage.FormatTS(now))
	return err
}

// createSetupToken returns a new setup token valid SetupTokenTTL.
func (s *store) createSetupToken(ctx context.Context) (string, time.Time, error) {
	if err := s.purge(ctx); err != nil {
		return "", time.Time{}, err
	}
	token, hash, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := s.now().Add(SetupTokenTTL)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO ui_setup_tokens (token_hash, expires_at) VALUES (?, ?)`, hash, storage.FormatTS(expires))
	return token, expires, err
}

// setupTokenValid reports whether token is unused and unexpired.
func (s *store) setupTokenValid(ctx context.Context, token string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM ui_setup_tokens WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		hashToken(token), storage.FormatTS(s.now())).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// useSetupToken marks token used; false if it was not valid.
func (s *store) useSetupToken(ctx context.Context, token string) (bool, error) {
	now := storage.FormatTS(s.now())
	res, err := s.db.ExecContext(ctx,
		`UPDATE ui_setup_tokens SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`,
		now, hashToken(token), now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
