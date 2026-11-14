package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
)

// Protocol is the server document's protocol name.
const Protocol = "conclave-federation/1"

// Identity is this server's federation identity: its domain, its
// current Ed25519 key, and all its keys.
type Identity struct {
	Domain  string
	KeyID   string
	Public  ed25519.PublicKey
	private ed25519.PrivateKey
	keys    []ownKey // every row of federation_keys, newest first
}

// ownKey is a row of federation_keys without its private key.
type ownKey struct {
	keyID      string
	public     ed25519.PublicKey
	retiredAt  time.Time    // zero while current
	endorsedBy *Endorsement // nil for a key not made by a rotation
}

// keyOverlap is how long a rotated-out key stays in the document.
const keyOverlap = 30 * 24 * time.Hour

// ErrRotationTooSoon is returned by Rotate while a previous key is
// still in its overlap.
var ErrRotationTooSoon = errors.New("the previous key is still in its 30-day overlap")

// LoadOrCreateIdentity returns the identity with the newest unretired
// key in federation_keys, generating and storing a key if there is
// none. The domain comes from configuration; it is not stored.
func LoadOrCreateIdentity(ctx context.Context, db *sql.DB, domain string) (*Identity, error) {
	const load = `SELECT private_key FROM federation_keys
		WHERE retired_at IS NULL ORDER BY created_at DESC, rowid DESC LIMIT 1`
	var priv []byte
	err := db.QueryRowContext(ctx, load).Scan(&priv)
	if errors.Is(err, sql.ErrNoRows) {
		pub, gen, genErr := ed25519.GenerateKey(rand.Reader)
		if genErr != nil {
			return nil, fmt.Errorf("generate federation key: %w", genErr)
		}
		// One statement, so two starts racing on one database cannot
		// both store a key; the loser reads the winner's.
		if _, err = db.ExecContext(ctx, `INSERT INTO federation_keys
			(key_id, public_key, private_key, created_at)
			SELECT ?, ?, ?, ? WHERE NOT EXISTS
			(SELECT 1 FROM federation_keys WHERE retired_at IS NULL)`,
			KeyIDFor(pub), []byte(pub), []byte(gen), storage.FormatTS(time.Now())); err != nil {
			return nil, fmt.Errorf("store federation key: %w", err)
		}
		err = db.QueryRowContext(ctx, load).Scan(&priv)
	}
	if err != nil {
		return nil, fmt.Errorf("load federation key: %w", err)
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("load federation key: stored private key has %d bytes", len(priv))
	}
	key := ed25519.PrivateKey(priv)
	pub := key.Public().(ed25519.PublicKey)
	id := &Identity{Domain: domain, KeyID: KeyIDFor(pub), Public: pub, private: key}
	rows, err := db.QueryContext(ctx, `SELECT key_id, public_key, COALESCE(retired_at, ''), COALESCE(endorsement, '')
		FROM federation_keys ORDER BY created_at DESC, rowid DESC`)
	if err != nil {
		return nil, fmt.Errorf("load federation keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k ownKey
		var pub []byte
		var retired, endorsement string
		if err := rows.Scan(&k.keyID, &pub, &retired, &endorsement); err != nil {
			return nil, fmt.Errorf("load federation keys: %w", err)
		}
		k.public = pub
		k.retiredAt, _ = time.Parse(time.RFC3339Nano, retired)
		if endorsement != "" {
			if err := json.Unmarshal([]byte(endorsement), &k.endorsedBy); err != nil {
				return nil, fmt.Errorf("load federation keys: endorsement of %s: %w", k.keyID, err)
			}
		}
		id.keys = append(id.keys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load federation keys: %w", err)
	}
	return id, nil
}

// Rotate makes a new key current in db and returns the identity with
// it. id's key, the current one, signs an endorsement of the new key
// and stays in the document until keyOverlap after now. While a key
// rotated out earlier is still in its overlap, it returns
// ErrRotationTooSoon, so a peer never needs more than one step.
func Rotate(ctx context.Context, db *sql.DB, id *Identity, now time.Time) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate federation key: %w", err)
	}
	keyID := KeyIDFor(pub)
	endorsedBy := Endorsement{KeyID: id.KeyID,
		Sig: b64.EncodeToString(id.Sign(EndorsementString(id.Domain, id.KeyID, keyID, b64.EncodeToString(pub))))}
	endorsement, _ := json.Marshal(endorsedBy)
	until := storage.FormatTS(now.Add(keyOverlap))
	// The new identity is built before the commit: after it, nothing that
	// can fail stands between the stored key and the one we sign with.
	next := &Identity{Domain: id.Domain, KeyID: keyID, Public: pub, private: priv,
		keys: []ownKey{{keyID: keyID, public: pub, endorsedBy: &endorsedBy}}}
	for _, k := range id.keys {
		if k.keyID == id.KeyID {
			k.retiredAt, _ = time.Parse(time.RFC3339Nano, until)
		}
		next.keys = append(next.keys, k)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE federation_keys SET retired_at = ? WHERE key_id = ? AND retired_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM federation_keys WHERE retired_at > ?)`,
		until, id.KeyID, storage.FormatTS(now))
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, ErrRotationTooSoon
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO federation_keys(key_id, public_key, private_key, created_at, endorsement)
		VALUES (?, ?, ?, ?, ?)`, keyID, []byte(pub), []byte(priv), storage.FormatTS(now), string(endorsement)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return next, nil
}

// ForgetOldKeys deletes every key in db but id's current one, private
// keys included, and returns the identity without them and the key IDs
// deleted: the document no longer lists them, and our bots' author
// signatures made with them no longer verify. The current key keeps its
// endorsement, so a peer still pinned to the key before it follows it.
//
// The whole table is cleared and the current key written back, with
// secure_delete on: SQLite then zeroes the table's pages, where an
// update or delete would leave old copies of a row in free space, and a
// checkpoint moves the zeroed pages into the database file and empties
// the write-ahead log. Backups made before keep the keys.
func ForgetOldKeys(ctx context.Context, db *sql.DB, id *Identity) (*Identity, []string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
		return nil, nil, err
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA secure_delete = OFF`) }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var dropped []string
	rows, err := tx.QueryContext(ctx, `SELECT key_id FROM federation_keys WHERE key_id != ? ORDER BY created_at DESC, rowid DESC`, id.KeyID)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		dropped = append(dropped, k)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	var pub, priv []byte
	var created string
	var endorsement sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT public_key, private_key, created_at, endorsement FROM federation_keys
		WHERE key_id = ? AND retired_at IS NULL`, id.KeyID).Scan(&pub, &priv, &created, &endorsement); err != nil {
		return nil, nil, fmt.Errorf("load the current federation key: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_keys`); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO federation_keys(key_id, public_key, private_key, created_at, endorsement)
		VALUES (?, ?, ?, ?, ?)`, id.KeyID, pub, priv, created, endorsement); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	// Best effort: a checkpoint blocked by readers leaves it to a later one.
	_, _ = conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	next := *id
	next.keys = slices.DeleteFunc(slices.Clone(id.keys), func(k ownKey) bool { return k.keyID != id.KeyID })
	return &next, dropped, nil
}

// EndorsementString is what a server's previous key signs for the key
// that replaces it: the lines conclave-key-v1, the domain, the previous
// key's ID, the new key's ID and the new public key (unpadded
// base64url), joined by \n.
func EndorsementString(domain, prevKeyID, keyID, publicKey string) []byte {
	return []byte(strings.Join([]string{"conclave-key-v1", domain, prevKeyID, keyID, publicKey}, "\n"))
}

// signed reports whether sig on m verifies with any of the identity's
// keys, the current one first: a message of our bots may be from
// before a rotation.
func (id *Identity) signed(m MessageSig, sig string) bool {
	return slices.ContainsFunc(id.keys, func(k ownKey) bool { return VerifyMessage(k.public, m, sig) })
}

// Sign signs msg with the identity's private key.
func (id *Identity) Sign(msg []byte) []byte { return ed25519.Sign(id.private, msg) }

// Fingerprint returns the identity's key fingerprint.
func (id *Identity) Fingerprint() string { return Fingerprint(id.Public) }

// Fingerprint returns "SHA256:" and the unpadded base64url SHA-256 of
// the public key, as SSH shows fingerprints (but base64url).
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "SHA256:" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// keyIDLen is the length of a key ID.
const keyIDLen = len("ed25519:") + 16

// KeyIDFor returns "ed25519:" and the first 16 characters of the
// fingerprint after "SHA256:".
func KeyIDFor(pub ed25519.PublicKey) string {
	return "ed25519:" + Fingerprint(pub)[len("SHA256:"):][:16]
}

// Document is the server document served at /.well-known/conclave.
type Document struct {
	Protocol string   `json:"protocol"`
	Domain   string   `json:"domain"`
	Keys     []DocKey `json:"keys"`
}

// DocKey is one key in a Document. A server before key rotation sends
// none of Status, Until and EndorsedBy.
type DocKey struct {
	KeyID       string       `json:"key_id"`
	Alg         string       `json:"alg"`
	PublicKey   string       `json:"public_key"` // unpadded base64url
	Fingerprint string       `json:"fingerprint"`
	Status      string       `json:"status,omitempty"`      // current or previous
	Until       string       `json:"until,omitempty"`       // a previous key's end, RFC 3339
	EndorsedBy  *Endorsement `json:"endorsed_by,omitempty"` // on a current key made by a rotation
}

// Endorsement is a previous key's signature, unpadded base64url, of
// EndorsementString for the key that replaced it.
type Endorsement struct {
	KeyID string `json:"key_id"`
	Sig   string `json:"sig"`
}

// Document returns the identity's server document at now: the current
// key, then each previous key until its retired_at, newest first.
func (id *Identity) Document(now time.Time) Document {
	doc := Document{Protocol: Protocol, Domain: id.Domain, Keys: []DocKey{docKey(id.Public, "current")}}
	for _, k := range id.keys {
		switch {
		case k.keyID == id.KeyID:
			doc.Keys[0].EndorsedBy = k.endorsedBy
		case k.retiredAt.After(now):
			dk := docKey(k.public, "previous")
			dk.Until = k.retiredAt.UTC().Format(time.RFC3339)
			doc.Keys = append(doc.Keys, dk)
		}
	}
	return doc
}

func docKey(pub ed25519.PublicKey, status string) DocKey {
	return DocKey{KeyID: KeyIDFor(pub), Alg: "ed25519", PublicKey: base64.RawURLEncoding.EncodeToString(pub),
		Fingerprint: Fingerprint(pub), Status: status}
}
