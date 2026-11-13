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

// External is the trust origin of text written outside the
// organisation: remote profiles and the keys marked external.
const External = "external"

// Profile is a bot's public face. KeyID is 0 for remote bots. UpdatedAt
// is zero without a saved row. TrustOrigin is the trust origin of the
// bot's key, External for a remote bot; federation answers leave it out.
type Profile struct {
	KeyID       int64     `json:"-"`
	Handle      string    `json:"handle"`
	DisplayName string    `json:"display_name"`
	Description string    `json:"description"`
	Skills      []Skill   `json:"skills"`
	Visibility  string    `json:"visibility"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	TrustOrigin string    `json:"trust_origin,omitempty"`
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
type Store struct {
	db        *sql.DB
	afterRead func() // test hook: runs in Update between the read and the write
}

// NewStore returns a Store on db.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const selectProfiles = `SELECT k.id, k.handle, COALESCE(p.display_name, ''), COALESCE(p.description, ''),
	COALESCE(p.skills, '[]'), COALESCE(p.visibility, 'private'), p.updated_at, k.trust_origin
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
		if err := rows.Scan(&p.KeyID, &p.Handle, &p.DisplayName, &p.Description, &skills, &p.Visibility, &updated, &p.TrustOrigin); err != nil {
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

// Update cleans the patched fields, checks the limits and writes them
// to the profile of handle, which it returns as stored. Fields not in
// the patch keep their stored value, in the same statement, so a stale
// read cannot undo another write. Only asAdmin may change the
// visibility.
func (s *Store) Update(ctx context.Context, handle string, patch Patch, asAdmin bool) (Profile, error) {
	if patch.Visibility != nil && !asAdmin {
		return Profile{}, ErrForbidden
	}
	cur, err := s.Get(ctx, handle)
	if err != nil {
		return Profile{}, err
	}
	if s.afterRead != nil {
		s.afterRead()
	}
	// p holds the patched values, and the defaults a new row gets.
	p := Profile{Skills: []Skill{}, Visibility: Private}
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
	skills, _ := json.Marshal(p.Skills)
	_, err = s.db.ExecContext(ctx, `INSERT INTO bot_profiles(key_id, display_name, description, skills, visibility, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_id) DO UPDATE SET
		display_name = CASE WHEN ? THEN excluded.display_name ELSE bot_profiles.display_name END,
		description = CASE WHEN ? THEN excluded.description ELSE bot_profiles.description END,
		skills = CASE WHEN ? THEN excluded.skills ELSE bot_profiles.skills END,
		visibility = CASE WHEN ? THEN excluded.visibility ELSE bot_profiles.visibility END,
		updated_at = excluded.updated_at`,
		cur.KeyID, p.DisplayName, p.Description, string(skills), p.Visibility, storage.FormatTS(time.Now()),
		patch.DisplayName != nil, patch.Description != nil, patch.Skills != nil, asAdmin && patch.Visibility != nil)
	if err != nil {
		return Profile{}, err
	}
	return s.ByKey(ctx, cur.KeyID)
}

var skillIDRE = regexp.MustCompile(`^[a-z0-9_.-]{1,32}$`)

// Profile limits, in characters.
const (
	maxDisplayName      = 64
	maxDescription      = 1000
	maxSkills           = 20
	maxSkillName        = 64
	maxSkillDescription = 300
)

func check(p Profile) error {
	long := func(s string, n int) bool { return utf8.RuneCountInString(s) > n }
	switch {
	case long(p.DisplayName, maxDisplayName):
		return fmt.Errorf("%w: display_name is longer than %d characters", ErrInvalid, maxDisplayName)
	case long(p.Description, maxDescription):
		return fmt.Errorf("%w: description is longer than %d characters", ErrInvalid, maxDescription)
	case len(p.Skills) > maxSkills:
		return fmt.Errorf("%w: more than %d skills", ErrInvalid, maxSkills)
	case !slices.Contains([]string{Private, Peers, Public}, p.Visibility):
		return fmt.Errorf("%w: visibility must be private, peers or public", ErrInvalid)
	}
	for _, sk := range p.Skills {
		switch {
		case !skillIDRE.MatchString(sk.ID):
			return fmt.Errorf("%w: skill id %q must be 1 to 32 of a-z, 0-9, '_', '.', '-'", ErrInvalid, sk.ID)
		case long(sk.Name, maxSkillName):
			return fmt.Errorf("%w: skill %s: name is longer than %d characters", ErrInvalid, sk.ID, maxSkillName)
		case long(sk.Description, maxSkillDescription):
			return fmt.Errorf("%w: skill %s: description is longer than %d characters", ErrInvalid, sk.ID, maxSkillDescription)
		}
	}
	return nil
}

// CleanRemote cleans a profile received from a peer like a saved one,
// but truncates it to the limits instead of refusing it. Skills with an
// invalid id, and those over the limit, are dropped; a visibility other
// than public reads as peers. ok is false for an invalid handle, a
// handle naming a server included: the caller drops the entry.
func CleanRemote(p Profile) (_ Profile, ok bool) {
	h, err := NormalizeHandle(p.Handle)
	if err != nil {
		return Profile{}, false
	}
	out := Profile{Handle: h, DisplayName: truncate(Clean(p.DisplayName, false), maxDisplayName),
		Description: truncate(Clean(p.Description, true), maxDescription),
		Skills:      []Skill{}, Visibility: Peers, UpdatedAt: p.UpdatedAt, TrustOrigin: External}
	if p.Visibility == Public {
		out.Visibility = Public
	}
	for _, sk := range p.Skills {
		if len(out.Skills) == maxSkills {
			break
		}
		if id := Clean(sk.ID, false); skillIDRE.MatchString(id) {
			out.Skills = append(out.Skills, Skill{ID: id, Name: truncate(Clean(sk.Name, false), maxSkillName),
				Description: truncate(Clean(sk.Description, true), maxSkillDescription)})
		}
	}
	return out, true
}

// truncate cuts s to n characters.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// SetHandle gives the active non-admin local key keyID the handle, which
// must be normalized; stand-ins of remote bots are not found. A handle in use, even by a revoked key, is ErrTaken.
func (s *Store) SetHandle(ctx context.Context, keyID int64, handle string) error {
	var admin bool
	err := s.db.QueryRowContext(ctx, `SELECT is_admin FROM api_keys WHERE id = ? AND revoked_at IS NULL AND remote_domain IS NULL`, keyID).Scan(&admin)
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
