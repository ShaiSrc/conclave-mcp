package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"

	"golang.org/x/time/rate"
)

const (
	maxBody          = 64 << 10         // request and response bodies
	fetchTimeout     = 5 * time.Second  // documents, pings, bot lookups; dialing and TLS of every request
	roomTimeout      = 30 * time.Second // room requests: invites, pushes, pulls, forwarded writes, leaves
	fetchesPerMinute = 10               // documents of domains that are not allowed, server-wide
)

// ErrFetchLimited is returned when the fetch budget for domains that
// are not allowed is used up.
var ErrFetchLimited = errors.New("too many federation document fetches, try again later")

var errNonPublicAddr = errors.New("refusing a non-public address")

// nonPublic are the ranges public refuses on top of what netip calls
// not global unicast or private. 6to4 and Teredo carry IPv4 addresses
// that cannot be checked reliably, so they are refused outright.
var nonPublic = prefixes(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "255.255.255.255/32",
	"::/96", "2001:db8::/32", "fec0::/10", "2002::/16", "2001::/32",
	// RFC 8215 local-use NAT64 prefixes may embed the IPv4 address at
	// other offsets, so they are refused outright.
	"64:ff9b:1::/48",
)

// nat64 is the well-known NAT64 prefix; it carries an IPv4 address in
// its last 32 bits.
var nat64 = prefixes("64:ff9b::/96")

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

func inAny(a netip.Addr, ps []netip.Prefix) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Fetcher makes the server's outgoing federation requests. For a
// domain that is not allowed, it connects only to public addresses.
type Fetcher struct {
	insecureHTTP bool
	isAllowed    func(domain string) bool
	limit        *rate.Limiter
	client       *http.Client // fetchTimeout
	roomClient   *http.Client // roomTimeout, the same transport
}

type allowPrivateKey struct{}

// NewFetcher returns a Fetcher that uses http:// with insecureHTTP and
// lets isAllowed domains have private addresses.
func NewFetcher(insecureHTTP bool, isAllowed func(domain string) bool) *Fetcher {
	f := &Fetcher{
		insecureHTTP: insecureHTTP,
		isAllowed:    isAllowed,
		limit:        rate.NewLimiter(rate.Every(time.Minute/fetchesPerMinute), fetchesPerMinute),
		client: &http.Client{
			Timeout:       fetchTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				// No Proxy: the dialer must see the peer's own address.
				DialContext: dial,
				// Every request dials, so no connection is reused
				// across domains with different address rules.
				DisableKeepAlives:      true,
				TLSHandshakeTimeout:    fetchTimeout,
				MaxResponseHeaderBytes: maxBody,
			},
		},
	}
	room := *f.client
	room.Timeout = roomTimeout
	f.roomClient = &room
	return f
}

func dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: fetchTimeout}
	if ok, _ := ctx.Value(allowPrivateKey{}).(bool); !ok {
		d.Control = refuseNonPublic
	}
	return d.DialContext(ctx, network, addr)
}

// refuseNonPublic is a net.Dialer Control hook. It sees the address
// actually connected, after DNS resolution, so a name that resolves or
// rebinds to an internal address is refused too.
func refuseNonPublic(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	if a := ap.Addr(); !public(a) {
		return fmt.Errorf("%w: %s", errNonPublicAddr, a)
	}
	return nil
}

// public reports whether a is a global unicast address outside every
// private and special range. A NAT64 address is judged by the IPv4
// address it carries.
func public(a netip.Addr) bool {
	a = a.Unmap()
	if inAny(a, nat64) {
		b := a.As16()
		return public(netip.AddrFrom4([4]byte(b[12:])))
	}
	return a.IsGlobalUnicast() && !a.IsPrivate() && !inAny(a, nonPublic)
}

// Document fetches and checks domain's server document. Domains that
// are not allowed share a budget of 10 fetches a minute.
func (f *Fetcher) Document(ctx context.Context, domain string) (Document, error) {
	allowed := f.isAllowed(domain)
	if !allowed && !f.limit.Allow() {
		return Document{}, ErrFetchLimited
	}
	return f.document(ctx, domain, allowed)
}

func (f *Fetcher) document(ctx context.Context, domain string, allowPrivate bool) (Document, error) {
	req, err := f.newRequest(ctx, http.MethodGet, domain, "/.well-known/conclave", nil)
	if err != nil {
		return Document{}, err
	}
	status, body, err := f.send(f.client, req, allowPrivate, maxBody)
	if err != nil {
		return Document{}, err
	}
	if status != http.StatusOK {
		return Document{}, fmt.Errorf("document of %s: status %d", domain, status)
	}
	var doc Document
	if err := json.Unmarshal(body, &doc); err != nil {
		return Document{}, fmt.Errorf("document of %s: %w", domain, err)
	}
	if doc.Protocol != Protocol || doc.Domain != domain || len(doc.Keys) == 0 {
		return Document{}, fmt.Errorf("document of %s: wrong protocol, domain or no keys", domain)
	}
	for _, k := range doc.Keys {
		pub, err := b64.DecodeString(k.PublicKey)
		if err != nil || len(pub) != ed25519.PublicKeySize || k.Alg != "ed25519" ||
			k.Fingerprint != Fingerprint(pub) || k.KeyID != KeyIDFor(pub) {
			return Document{}, fmt.Errorf("document of %s: key %q does not match its fingerprint", domain, k.KeyID)
		}
	}
	return doc, nil
}

func (f *Fetcher) newRequest(ctx context.Context, method, domain, path string, body []byte) (*http.Request, error) {
	// A canonical domain has no '/', '@' or '?' that could move the host.
	if d, err := NormalizeDomain(domain); err != nil || d != domain {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDomain, domain)
	}
	scheme := "https"
	if f.insecureHTTP {
		scheme = "http"
	}
	return http.NewRequestWithContext(ctx, method, scheme+"://"+domain+path, bytes.NewReader(body))
}

// send does req with client and returns the status and body, read up
// to limit bytes.
func (f *Fetcher) send(client *http.Client, req *http.Request, allowPrivate bool, limit int) (int, []byte, error) {
	req = req.WithContext(context.WithValue(req.Context(), allowPrivateKey{}, allowPrivate))
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return 0, nil, err
	}
	if len(body) > limit {
		return 0, nil, fmt.Errorf("response from %s is over %d bytes", req.URL.Host, limit)
	}
	return resp.StatusCode, body, nil
}

// Ping sends a signed ping to domain and returns the round-trip time.
func (s *Service) Ping(ctx context.Context, domain string) (time.Duration, error) {
	body := []byte("{}")
	req, err := s.fetch.newRequest(ctx, http.MethodPost, domain, "/fed/v1/ping", body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := s.id.SignRequest(req, domain, body); err != nil {
		return 0, err
	}
	start := time.Now()
	status, resp, err := s.fetch.send(s.fetch.client, req, s.fetch.isAllowed(domain), maxBody)
	rtt := time.Since(start)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		var env struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(resp, &env)
		return 0, fmt.Errorf("ping %s: status %d %s", domain, status, env.Error.Code)
	}
	var out struct {
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(resp, &out); err != nil || out.Domain != domain {
		return 0, fmt.Errorf("ping %s: answer from another domain or not JSON", domain)
	}
	return rtt, nil
}
