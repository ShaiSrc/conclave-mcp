package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sigScheme   = "Conclave-Sig-v1"
	maxParamLen = 512
)

var errBadSigHeader = errors.New("malformed signature header")

// b64 is unpadded base64url; Strict refuses non-zero trailing bits.
var b64 = base64.RawURLEncoding.Strict()

// decodeB64 decodes s, which must be b64 in its one spelling: the
// decoder also skips CR and LF, and a signature or nonce is stored as
// it is spelled, to find it again.
func decodeB64(s string) ([]byte, error) {
	b, err := b64.DecodeString(s)
	if err == nil && b64.EncodeToString(b) != s {
		err = errors.New("base64 not in its canonical form")
	}
	return b, err
}

// SigParams are the parameters of a Conclave-Sig-v1 Authorization header.
type SigParams struct {
	Origin, KeyID, Nonce string
	TS                   int64
	Sig                  []byte
}

// ParseSigHeader parses `Conclave-Sig-v1 origin="…",key="…",ts="…",nonce="…",sig="…"`.
// Each of the five parameters must appear exactly once, in any order,
// separated by bare commas, each value quoted, without escapes and at
// most 512 bytes. ts is a canonical decimal, nonce 16 bytes and sig 64
// bytes, both unpadded base64url in its one spelling. The origin's
// syntax is not checked here.
func ParseSigHeader(h string) (SigParams, error) {
	rest, ok := strings.CutPrefix(h, sigScheme+" ")
	if !ok {
		return SigParams{}, errBadSigHeader
	}
	vals := make(map[string]string, 5)
	for {
		name, after, ok := strings.Cut(rest, `="`)
		if !ok {
			return SigParams{}, errBadSigHeader
		}
		val, after, ok := strings.Cut(after, `"`)
		if !ok || len(val) > maxParamLen || strings.Contains(val, `\`) {
			return SigParams{}, errBadSigHeader
		}
		switch name {
		case "origin", "key", "ts", "nonce", "sig":
		default:
			return SigParams{}, errBadSigHeader
		}
		if _, dup := vals[name]; dup {
			return SigParams{}, errBadSigHeader
		}
		vals[name] = val
		if after == "" {
			break
		}
		if rest, ok = strings.CutPrefix(after, ","); !ok {
			return SigParams{}, errBadSigHeader
		}
	}
	if len(vals) != 5 {
		return SigParams{}, errBadSigHeader
	}

	p := SigParams{Origin: vals["origin"], KeyID: vals["key"], Nonce: vals["nonce"]}
	var err error
	if p.TS, err = strconv.ParseInt(vals["ts"], 10, 64); err != nil || strconv.FormatInt(p.TS, 10) != vals["ts"] {
		return SigParams{}, errBadSigHeader
	}
	if n, err := decodeB64(p.Nonce); err != nil || len(n) != 16 {
		return SigParams{}, errBadSigHeader
	}
	if p.Sig, err = decodeB64(vals["sig"]); err != nil || len(p.Sig) != ed25519.SignatureSize {
		return SigParams{}, errBadSigHeader
	}
	return p, nil
}

// SigningString returns the bytes a request signature covers: the
// lines conclave-sig-v1, method, path with query, destination, origin,
// ts, nonce and the lowercase hex SHA-256 of the body, joined by \n.
func SigningString(method, pathQuery, destination, origin string, ts int64, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{"conclave-sig-v1", method, pathQuery, destination, origin,
		strconv.FormatInt(ts, 10), nonce, hex.EncodeToString(sum[:])}, "\n"))
}

// SignRequest signs req, whose body is body, for the server at
// destination and sets its Authorization header.
func (id *Identity) SignRequest(req *http.Request, destination string, body []byte) error {
	ts, nonce := time.Now().Unix(), newNonce()
	sig := id.Sign(SigningString(req.Method, req.URL.RequestURI(), destination, id.Domain, ts, nonce, body))
	req.Header.Set("Authorization", formatSigHeader(SigParams{Origin: id.Domain, KeyID: id.KeyID, TS: ts, Nonce: nonce, Sig: sig}))
	return nil
}

func formatSigHeader(p SigParams) string {
	return fmt.Sprintf(`%s origin="%s",key="%s",ts="%d",nonce="%s",sig="%s"`,
		sigScheme, p.Origin, p.KeyID, p.TS, p.Nonce, b64.EncodeToString(p.Sig))
}

// newNonce returns 16 random bytes as 22 characters of base64url.
func newNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	return b64.EncodeToString(b[:])
}
