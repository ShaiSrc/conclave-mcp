package e2e

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// follow is a follow as the follow endpoints answer it.
type follow struct {
	ID     int64  `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"`
}

// tlPost is a post as GET /v1/timeline lists it.
type tlPost struct {
	ID            int64  `json:"id"`
	RoomID        string `json:"room_id"`
	AuthorAddress string `json:"author_address"`
	Content       string `json:"content"`
	Visibility    string `json:"visibility"`
	TrustOrigin   string `json:"trust_origin"`
	Unverified    bool   `json:"unverified"`
}

// follows returns the follows k reads at path on f, listed under key.
func (f *fedEnv) follows(k apiKey, path, key string) []follow {
	f.t.Helper()
	r := f.callAs("GET", path, k, nil)
	var out map[string][]follow
	if r.Status != 200 || json.Unmarshal(r.Body, &out) != nil {
		f.t.Fatalf("%s on %s: %d %s", path, f.domain, r.Status, r.Body)
	}
	return out[key]
}

// timeline returns k's timeline on f, by content.
func (f *fedEnv) timeline(k apiKey) map[string]tlPost {
	f.t.Helper()
	r := f.callAs("GET", "/v1/timeline?limit=100", k, nil)
	var out struct{ Posts []tlPost }
	if r.Status != 200 || json.Unmarshal(r.Body, &out) != nil {
		f.t.Fatalf("timeline on %s: %d %s", f.domain, r.Status, r.Body)
	}
	m := map[string]tlPost{}
	for _, p := range out.Posts {
		m[p.Content] = p
	}
	return m
}

// post posts content with visibility as k on f and returns its message id
// and room.
func (f *fedEnv) post(k apiKey, content, visibility string) (int64, string) {
	f.t.Helper()
	r := f.callAs("POST", "/v1/posts", k, map[string]string{"content": content, "visibility": visibility})
	var m struct {
		ID     int64  `json:"id"`
		RoomID string `json:"room_id"`
	}
	if r.Status != 201 || json.Unmarshal(r.Body, &m) != nil || m.ID == 0 {
		f.t.Fatalf("post on %s: %d %s", f.domain, r.Status, r.Body)
	}
	return m.ID, m.RoomID
}

// Bot a1 on A follows b1 on B (policy approve), b1 accepts and posts,
// the post reaches a1's timeline and its delete follows it; with policy
// none, a2 on A is refused; a public post of b1 shows on B's public
// page and Atom feed.
func TestFollow_AcrossServers(t *testing.T) {
	a, b := newFedEnv(t), newFedEnv(t)
	a.peer(b)
	b.peer(a)
	a1, a2, b1 := a.botKey("a1"), a.botKey("a2"), b.botKey("b1")
	a1Addr, b1Addr := "a1@"+a.domain, "b1@"+b.domain
	if r := b.admin("GET", "/v1/bots/b1", nil); r.Status != 200 || !strings.Contains(string(r.Body), `"follow_policy":"approve"`) {
		t.Fatalf("b1's default policy: %d %s", r.Status, r.Body)
	}

	// 1. a1 follows b1: pending on A, a pending request on B.
	r := a.callAs("POST", "/v1/follows", a1, map[string]string{"to": b1Addr})
	var f follow
	if r.Status != 201 || json.Unmarshal(r.Body, &f) != nil || f.From != a1Addr || f.To != b1Addr || f.Status != "pending" {
		t.Fatalf("follow: %d %s", r.Status, r.Body)
	}
	reqs := b.follows(b1, "/v1/follows/requests", "requests")
	if len(reqs) != 1 || reqs[0].From != a1Addr || reqs[0].To != b1Addr || reqs[0].Status != "pending" {
		t.Fatalf("requests on B: %+v", reqs)
	}

	// 2. b1 accepts: B invites A into b1's feed, and a1's follow is accepted.
	r = b.callAs("POST", "/v1/follows/requests/"+strconv.FormatInt(reqs[0].ID, 10)+"/accept", b1, nil)
	if r.Status != 200 || !strings.Contains(string(r.Body), `"status":"accepted"`) {
		t.Fatalf("accept: %d %s", r.Status, r.Body)
	}
	waitFor(t, "a1's follow accepted", func() bool {
		fs := a.follows(a1, "/v1/follows", "follows")
		return len(fs) == 1 && fs[0].To == b1Addr && fs[0].Status == "accepted"
	})
	if fs := b.follows(b1, "/v1/followers", "followers"); len(fs) != 1 || fs[0].From != a1Addr {
		t.Fatalf("followers on B: %+v", fs)
	}

	// 3. b1 posts: the post reaches a1's timeline from b1's address,
	// external, in A's replica of the feed.
	id, feed := b.post(b1, "hello followers", "followers")
	var p tlPost
	waitFor(t, "the post on A", func() bool { var ok bool; p, ok = a.timeline(a1)["hello followers"]; return ok })
	if p.AuthorAddress != b1Addr || p.TrustOrigin != "external" || p.Unverified || p.Visibility != "followers" || p.RoomID != feed {
		t.Fatalf("post on A: %+v", p)
	}
	rooms := a.callAs("GET", "/v1/rooms?kind=feed", a1, nil)
	if rooms.Status != 200 || !strings.Contains(string(rooms.Body), `"feed":"`+b1Addr+`"`) || !strings.Contains(string(rooms.Body), `"home":"`+b.domain+`"`) {
		t.Fatalf("a1's feeds: %d %s", rooms.Status, rooms.Body)
	}

	// 4. b1 deletes the post: it leaves a1's timeline.
	if r := b.callAs("DELETE", "/v1/posts/"+strconv.FormatInt(id, 10), b1, nil); r.Status != 204 {
		t.Fatalf("delete: %d %s", r.Status, r.Body)
	}
	waitFor(t, "the delete on A", func() bool { _, ok := a.timeline(a1)["hello followers"]; return !ok })

	// 5. Policy none: a2 is refused; a1 keeps its follow.
	if r := b.admin("PATCH", "/v1/bots/b1/profile", map[string]string{"follow_policy": "none"}); r.Status != 200 {
		t.Fatalf("set policy: %d %s", r.Status, r.Body)
	}
	if r := a.callAs("POST", "/v1/follows", a2, map[string]string{"to": b1Addr}); r.Status != 403 || !strings.Contains(string(r.Body), "follow_refused") {
		t.Fatalf("refused follow: %d %s", r.Status, r.Body)
	}
	if fs := a.follows(a2, "/v1/follows", "follows"); len(fs) != 0 {
		t.Fatalf("a2 follows: %+v", fs)
	}
	if na, nb := a.auditCount("follow.refused"), b.auditCount("follow.refused"); na != 1 || nb != 1 {
		t.Fatalf("follow.refused: %d on A, %d on B", na, nb)
	}
	if fs := b.follows(b1, "/v1/followers", "followers"); len(fs) != 1 || fs[0].From != a1Addr {
		t.Fatalf("followers on B after none: %+v", fs)
	}

	// 6. A public post of a public b1: on B's public page and feed, and in
	// a1's timeline; a followers-only post on neither public surface.
	if r := b.admin("PATCH", "/v1/bots/b1/profile", map[string]string{"visibility": "public"}); r.Status != 200 {
		t.Fatalf("make public: %d %s", r.Status, r.Body)
	}
	b.post(b1, "only for followers", "followers")
	b.post(b1, "a public <post> & more", "public")
	waitFor(t, "the public post on A", func() bool { p, ok := a.timeline(a1)["a public <post> & more"]; return ok && p.Visibility == "public" })
	status, _, page := b.fedGet("/@b1")
	if status != 200 || !strings.Contains(page, "a public &lt;post&gt; &amp; more") || strings.Contains(page, "only for followers") {
		t.Fatalf("public page: %d %s", status, page)
	}
	status, hdr, atom := b.fedGet("/@b1/feed.xml")
	if status != 200 || hdr.Get("Content-Type") != "application/atom+xml; charset=utf-8" ||
		!strings.Contains(atom, "a public &lt;post&gt; &amp; more") || strings.Contains(atom, "only for followers") {
		t.Fatalf("atom feed: %d %v %s", status, hdr, atom)
	}
}
