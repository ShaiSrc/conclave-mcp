package bots

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hermes/conclave-mcp/internal/auth"
	"github.com/hermes/conclave-mcp/internal/storage"
)

// Visibility values, from least to most visible.
const (
	Private = "private" // admin keys and the bot itself
	Peers   = "peers"   // every key on this server and federation peers
	Public  = "public"  // also the public web
)

var rank = map[string]int{Private: 0, Peers: 1, Public: 2}

var (
	ErrNotFound  = errors.New("no such bot")
	ErrInvalid   = errors.New("invalid")
	ErrTaken     = errors.New("handle already taken")
	ErrAdminKey  = errors.New("admin keys cannot have a handle")
	ErrForbidden = errors.New("only an admin key changes the visibility")
)

// Skill is one thing a bot can do.
type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Profile is a bot's public face. KeyID is 0 for remote bots.
type Profile struct {
	KeyID       int64     `json:"-"`
	Handle      string    `json:"handle"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`
	Skills      []Skill   `json:"skills"`
	Visibility  string    `json:"visibility"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Patch changes the fields that are not nil.
type Patch struct {
	DisplayName *string  `json:"display_name"`
	Description *string  `json:"description"`
	Skills      *[]Skill `json:"skills"`
	Visibility  *string  `json:"visibility"`
}

// Visible reports whether viewer may see p: admin keys see every bot,
// a bot sees its own profile, other keys see peers and public ones.
func Visible(viewer auth.Claims, p Profile) bool {
	return slices.Contains(viewer.Scopes, auth.ScopeAdmin) ||
		(p.KeyID != 0 && viewer.KeyID == p.KeyID) ||
		rank[p.Visibility] >= rank[Peers]
}

// Store reads and writes handles and profiles. Revoked keys are hidden.
type Store struct{ db *sql.DB }

// NewStore returns a Store on db.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const selectProfiles = `SELECT k.id, k.handle, COALESCE(p.display_name, ''), COALESCE(p.description, ''),
	COALESCE(p.skills, '[]'), COALESCE(p.visibility, 'private'), p.updated_at
	FROM api_keys k LEFT JOIN bot_profiles p ON p.key_id = k.id
	WHERE k.handle IS NOT NULL AND k.revoked_at IS NULL `

func (s *Store) query(ctx context.Context, where string, args ...any) ([]Profile, error) {
	rows, err := s.db.QueryContext(ctx, selectProfiles+where+` ORDER BY k.handle`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Profile
	for rows.Next() {
		var (
			p       Profile
			skills  string
			updated sql.NullString
		)
		if err := rows.Scan(&p.KeyID, &p.Handle, &p.DisplayName, &p.Description, &skills, &p.Visibility, &updated); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(skills), &p.Skills); err != nil {
			return nil, err
		}
		if updated.Valid {
			if p.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated.String); err != nil {
				return nil, err
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) one(ctx context.Context, where string, args ...any) (Profile, error) {
	ps, err := s.query(ctx, where, args...)
	if err != nil {
		return Profile{}, err
	}
	if len(ps) == 0 {
		return Profile{}, ErrNotFound
	}
	return ps[0], nil
}

// Get returns the profile of the bot with handle.
func (s *Store) Get(ctx context.Context, handle string) (Profile, error) {
	return s.one(ctx, `AND k.handle = ?`, handle)
}

// ByKey returns the profile of the bot with key keyID.
func (s *Store) ByKey(ctx context.Context, keyID int64) (Profile, error) {
	return s.one(ctx, `AND k.id = ?`, keyID)
}

// List returns the bots at least as visible as minVisibility, ordered
// by handle.
func (s *Store) List(ctx context.Context, minVisibility string) ([]Profile, error) {
	var vis []any
	for v, r := range rank {
		if r >= rank[minVisibility] {
			vis = append(vis, v)
		}
	}
	return s.query(ctx, `AND COALESCE(p.visibility, 'private') IN (?`+strings.Repeat(`, ?`, len(vis)-1)+`)`, vis...)
}

// Update cleans the patched fields, checks the limits and stores the
// profile of handle. Only asAdmin may change the visibility.
func (s *Store) Update(ctx context.Context, handle string, patch Patch, asAdmin bool) (Profile, error) {
	if patch.Visibility != nil && !asAdmin {
		return Profile{}, ErrForbidden
	}
	p, err := s.Get(ctx, handle)
	if err != nil {
		return Profile{}, err
	}
	if patch.DisplayName != nil {
		p.DisplayName = Clean(*patch.DisplayName, false)
	}
	if patch.Description != nil {
		p.Description = Clean(*patch.Description, true)
	}
	if patch.Skills != nil {
		p.Skills = make([]Skill, len(*patch.Skills))
		for i, sk := range *patch.Skills {
			p.Skills[i] = Skill{ID: Clean(sk.ID, false), Name: Clean(sk.Name, false), Description: Clean(sk.Description, true)}
		}
	}
	if patch.Visibility != nil {
		p.Visibility = *patch.Visibility
	}
	if err := check(p); err != nil {
		return Profile{}, err
	}
	p.UpdatedAt = time.Now().UTC()
	skills, _ := json.Marshal(p.Skills)
	_, err = s.db.ExecContext(ctx, `INSERT INTO bot_profiles(key_id, display_name, description, skills, visibility, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_id) DO UPDATE SET display_name = excluded.display_name, description = excluded.description,
		skills = excluded.skills, visibility = excluded.visibility, updated_at = excluded.updated_at`,
		p.KeyID, p.DisplayName, p.Description, string(skills), p.Visibility, storage.FormatTS(p.UpdatedAt))
	if err != nil {
		return Profile{}, err
	}
	return p, nil
}

var skillIDRE = regexp.MustCompile(`^[a-z0-9_.-]{1,32}$`)

func check(p Profile) error {
	long := func(s string, n int) bool { return utf8.RuneCountInString(s) > n }
	switch {
	case long(p.DisplayName, 64):
		return fmt.Errorf("%w: display_name is longer than 64 characters", ErrInvalid)
	case long(p.Description, 1000):
		return fmt.Errorf("%w: description is longer than 1000 characters", ErrInvalid)
	case len(p.Skills) > 20:
		return fmt.Errorf("%w: more than 20 skills", ErrInvalid)
	case !slices.Contains([]string{Private, Peers, Public}, p.Visibility):
		return fmt.Errorf("%w: visibility must be private, peers or public", ErrInvalid)
	}
	for _, sk := range p.Skills {
		switch {
		case !skillIDRE.MatchString(sk.ID):
			return fmt.Errorf("%w: skill id %q must be 1 to 32 of a-z, 0-9, '_', '.', '-'", ErrInvalid, sk.ID)
		case long(sk.Name, 64):
			return fmt.Errorf("%w: skill %s: name is longer than 64 characters", ErrInvalid, sk.ID)
		case long(sk.Description, 300):
			return fmt.Errorf("%w: skill %s: description is longer than 300 characters", ErrInvalid, sk.ID)
		}
	}
	return nil
}

// SetHandle gives the active non-admin key keyID the handle, which must
// be normalized. A handle in use, even by a revoked key, is ErrTaken.
func (s *Store) SetHandle(ctx context.Context, keyID int64, handle string) error {
	var admin bool
	err := s.db.QueryRowContext(ctx, `SELECT is_admin FROM api_keys WHERE id = ? AND revoked_at IS NULL`, keyID).Scan(&admin)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w (no active key %d)", ErrNotFound, keyID)
	case err != nil:
		return err
	case admin:
		return ErrAdminKey
	}
	_, err = s.db.ExecContext(ctx, `UPDATE api_keys SET handle = ? WHERE id = ?`, handle, keyID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrTaken
	}
	return err
}

// RemoveHandle takes the handle from the active key keyID, deletes its
// profile so a later handle starts private, and returns the old handle.
func (s *Store) RemoveHandle(ctx context.Context, keyID int64) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var handle string
	err = tx.QueryRowContext(ctx, `SELECT handle FROM api_keys
		WHERE id = ? AND revoked_at IS NULL AND handle IS NOT NULL`, keyID).Scan(&handle)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w (no active key %d with a handle)", ErrNotFound, keyID)
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET handle = NULL WHERE id = ?`, keyID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM bot_profiles WHERE key_id = ?`, keyID); err != nil {
		return "", err
	}
	return handle, tx.Commit()
}
