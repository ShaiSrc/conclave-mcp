package federation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/hermes/conclave-mcp/internal/fedrooms"
)

// vectorsFile holds the test vectors of protocol version 1.
const vectorsFile = "../../docs/protocol-v1-vectors.json"

// protocolVectors is the shape of vectorsFile.
type protocolVectors struct {
	Comment string    `json:"comment"`
	Key     vectorKey `json:"key"`
	Request struct {
		Method        string `json:"method"`
		PathQuery     string `json:"path_query"`
		Destination   string `json:"destination"`
		Origin        string `json:"origin"`
		TS            int64  `json:"ts"`
		Nonce         string `json:"nonce"`
		Body          string `json:"body"`
		BodySHA256    string `json:"body_sha256"`
		SigningString string `json:"signing_string"`
		Signature     string `json:"signature"`
		Authorization string `json:"authorization"`
	} `json:"request"`
	Endorsement struct {
		Domain        string    `json:"domain"`
		PreviousKeyID string    `json:"previous_key_id"`
		Key           vectorKey `json:"key"`
		SigningString string    `json:"signing_string"`
		Signature     string    `json:"signature"`
	} `json:"endorsement"`
	AuthorSignature struct {
		Room          string `json:"room"`
		Author        string `json:"author"`
		ContentType   string `json:"content_type"`
		MessageType   string `json:"message_type"`
		Content       string `json:"content"`
		Metadata      string `json:"metadata"`
		ReplyTo       *int64 `json:"reply_to"`
		Nonce         string `json:"nonce"`
		SigningString string `json:"signing_string"`
		Signature     string `json:"signature"`
	} `json:"author_signature"`
}

// vectorKey is an Ed25519 key of the vectors: its 32-byte seed (hex)
// and the values derived from it.
type vectorKey struct {
	Seed        string `json:"seed"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	KeyID       string `json:"key_id"`
}

// vectorIdentity is the identity of domain with the key of seed, a hex
// Ed25519 seed.
func vectorIdentity(t *testing.T, seed, domain string) *Identity {
	t.Helper()
	b, err := hex.DecodeString(seed)
	if err != nil || len(b) != ed25519.SeedSize {
		t.Fatalf("seed %q: %v", seed, err)
	}
	priv := ed25519.NewKeyFromSeed(b)
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{Domain: domain, KeyID: KeyIDFor(pub), Public: pub, private: priv}
}

// TestProtocolVectors recomputes every value of the vectors file from
// its seeds with this package's own functions: a change of the wire
// format, or of a vector, fails it. Ed25519 signatures are
// deterministic, so the values are exact.
func TestProtocolVectors(t *testing.T) {
	raw, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Fatal(err)
	}
	var v protocolVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	eq := func(name, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, want)
		}
	}
	key := func(name string, k vectorKey, domain string) *Identity {
		t.Helper()
		id := vectorIdentity(t, k.Seed, domain)
		eq(name+".public_key", b64.EncodeToString(id.Public), k.PublicKey)
		eq(name+".fingerprint", Fingerprint(id.Public), k.Fingerprint)
		eq(name+".key_id", id.KeyID, k.KeyID)
		return id
	}

	q := v.Request
	id := key("key", v.Key, q.Origin)
	sum := sha256.Sum256([]byte(q.Body))
	eq("request.body_sha256", hex.EncodeToString(sum[:]), q.BodySHA256)
	ss := SigningString(q.Method, q.PathQuery, q.Destination, q.Origin, q.TS, q.Nonce, []byte(q.Body))
	eq("request.signing_string", string(ss), q.SigningString)
	sig := id.Sign(ss)
	eq("request.signature", b64.EncodeToString(sig), q.Signature)
	eq("request.authorization", formatSigHeader(SigParams{Origin: q.Origin, KeyID: id.KeyID, TS: q.TS, Nonce: q.Nonce, Sig: sig}),
		q.Authorization)
	if p, err := ParseSigHeader(q.Authorization); err != nil || p.Origin != q.Origin || p.KeyID != id.KeyID || p.TS != q.TS ||
		p.Nonce != q.Nonce || b64.EncodeToString(p.Sig) != q.Signature {
		t.Errorf("request.authorization parses as %+v, %v", p, err)
	}

	e := v.Endorsement
	eq("endorsement.previous_key_id", id.KeyID, e.PreviousKeyID)
	next := key("endorsement.key", e.Key, e.Domain)
	es := EndorsementString(e.Domain, e.PreviousKeyID, next.KeyID, b64.EncodeToString(next.Public))
	eq("endorsement.signing_string", string(es), e.SigningString)
	eq("endorsement.signature", b64.EncodeToString(id.Sign(es)), e.Signature)

	a := v.AuthorSignature
	if wire, _, err := fedrooms.SplitMetadata([]byte(a.Metadata)); err != nil || string(wire) != a.Metadata {
		t.Errorf("author_signature.metadata %q is not in its wire form (%q, %v)", a.Metadata, wire, err)
	}
	m := MessageSig{Room: a.Room, Author: a.Author, ContentType: a.ContentType, MessageType: a.MessageType, Content: a.Content,
		Metadata: []byte(a.Metadata), ReplyTo: a.ReplyTo, Nonce: a.Nonce}
	eq("author_signature.signing_string", string(m.SigningString()), a.SigningString)
	eq("author_signature.signature", id.SignMessage(m), a.Signature)
	if !VerifyMessage(id.Public, m, a.Signature) {
		t.Error("author_signature.signature does not verify")
	}
}
