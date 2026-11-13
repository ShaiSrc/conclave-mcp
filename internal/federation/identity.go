package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/hermes/conclave-mcp/internal/storage"
)

// Protocol is the server document's protocol name.
const Protocol = "conclave-federation/1"

// Identity is this server's federation identity: its domain and its
// current Ed25519 key.
type Identity struct {
	Domain  string
	KeyID   string
	Public  ed25519.PublicKey
	private ed25519.PrivateKey
}

// LoadOrCreateIdentity returns the identity with the newest unretired
// key in federation_keys, generating and storing a key if there is
// none. The domain comes from configuration; it is not stored.
func LoadOrCreateIdentity(ctx context.Context, db *sql.DB, domain string) (*Identity, error) {
	var priv []byte
	err := db.QueryRowContext(ctx, `SELECT private_key FROM federation_keys
		WHERE retired_at IS NULL ORDER BY created_at DESC, rowid DESC LIMIT 1`).Scan(&priv)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		var pub ed25519.PublicKey
		if pub, priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
			return nil, fmt.Errorf("generate federation key: %w", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO federation_keys
			(key_id, public_key, private_key, created_at) VALUES (?, ?, ?, ?)`,
			KeyIDFor(pub), []byte(pub), priv, storage.FormatTS(time.Now())); err != nil {
			return nil, fmt.Errorf("store federation key: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("load federation key: %w", err)
	case len(priv) != ed25519.PrivateKeySize:
		return nil, fmt.Errorf("load federation key: stored private key has %d bytes", len(priv))
	}
	key := ed25519.PrivateKey(priv)
	pub := key.Public().(ed25519.PublicKey)
	return &Identity{Domain: domain, KeyID: KeyIDFor(pub), Public: pub, private: key}, nil
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

// DocKey is one key in a Document.
type DocKey struct {
	KeyID       string `json:"key_id"`
	Alg         string `json:"alg"`
	PublicKey   string `json:"public_key"` // unpadded base64url
	Fingerprint string `json:"fingerprint"`
}

// Document returns the identity's server document.
func (id *Identity) Document() Document {
	return Document{Protocol: Protocol, Domain: id.Domain, Keys: []DocKey{{
		KeyID:       id.KeyID,
		Alg:         "ed25519",
		PublicKey:   base64.RawURLEncoding.EncodeToString(id.Public),
		Fingerprint: id.Fingerprint(),
	}}}
}
