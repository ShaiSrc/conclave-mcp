package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Servers A, B and C each allow the other two. One scenario crosses all
// three, step by step: lookup and directory; a room on A with bots of B
// and C; a direct conversation of B's and C's bots; bots of B and C
// following a bot of A; C down and catching up; B rotating its key; A
// blocking a bot of C, then blocking and purging C.
func TestInterop_ThreeServers(t *testing.T) {
	root := t
	a, b, c := newFedEnv(t), newFedEnv(t), newFedEnv(t)
	envs := []*fedEnv{a, b, c}
	a1, b1, b2, c1, c2 := a.botKey("a1"), b.botKey("b1"), b.botKey("b2"), c.botKey("c1"), c.botKey("c2")
	a1Addr, b1Addr, b2Addr, c1Addr, c2Addr := "a1@"+a.domain, "b1@"+b.domain, "b2@"+b.domain, "c1@"+c.domain, "c2@"+c.domain
	handles := map[*fedEnv][]string{a: {"a1"}, b: {"b1", "b2"}, c: {"c1", "c2"}}
	for f, hs := range handles {
		for _, h := range hs {
			if r := f.admin("PATCH", "/v1/bots/"+h+"/profile", map[string]string{"visibility": "peers"}); r.Status != 200 {
				t.Fatalf("profile of %s: %d %s", h, r.Status, r.Body)
			}
		}
	}
	if r := c.admin("PATCH", "/v1/bots/c1/profile", map[string]string{"dm_policy": "peers"}); r.Status != 200 {
		t.Fatalf("c1's DM policy: %d %s", r.Status, r.Body)
	}
	// The first server of a pair to add the other has its directory
	// refresh refused, as the other does not allow it yet; adding the
	// peer again, as an operator may, refreshes it.
	for range 2 {
		for _, p := range [][2]*fedEnv{{a, b}, {b, a}, {a, c}, {c, a}, {b, c}, {c, b}} {
			p[0].peer(p[1])
		}
	}

	// step runs fn as a subtest whose failures the servers' helpers
	// report, and ends the test when it fails: each step builds on the
	// ones before.
	step := func(name string, fn func(t *testing.T)) {
		ok := t.Run(name, func(t *testing.T) {
			for _, f := range envs {
				f.t = t
			}
			defer func() {
				for _, f := range envs {
					f.t = root
				}
			}()
			fn(t)
		})
		if !ok {
			root.FailNow()
		}
	}

	var proj string
	say := func(f *fedEnv, k apiKey, room, content string) {
		f.t.Helper()
		r := f.callAs("POST", "/v1/rooms/"+room+"/messages", k, map[string]string{"content": content, "message_type": "answer"})
		if r.Status != 201 {
			f.t.Fatalf("post %q on %s: %d %s", content, f.domain, r.Status, r.Body)
		}
	}
	members := func(f *fedEnv) string {
		f.t.Helper()
		r := f.admin("GET", "/v1/rooms/"+proj+"/members", nil)
		if r.Status != 200 {
			f.t.Fatalf("members on %s: %d %s", f.domain, r.Status, r.Body)
		}
		return string(r.Body)
	}
	type reader struct {
		f *fedEnv
		k apiKey
	}
	all := []reader{{a, a1}, {b, b1}, {c, c1}}
	// inOrder waits until each reader lists proj's messages as want.
	inOrder := func(t *testing.T, want []string, rs ...reader) {
		t.Helper()
		for _, r := range rs {
			var got []string
			defer func() {
				if t.Failed() {
					t.Logf("%s lists %q", r.f.domain, got)
				}
			}()
			waitFor(t, fmt.Sprintf("%q on %s", want, r.f.domain), func() bool {
				got = got[:0]
				for _, m := range r.f.messageList(r.k, proj) {
					got = append(got, m.Content)
				}
				return slices.Equal(got, want)
			})
		}
	}
	// external checks content as k reads it in room on f: from author's
	// address, external, its author signature verified.
	external := func(f *fedEnv, k apiKey, room, content, author string) {
		f.t.Helper()
		if m, ok := f.messages(k, room)[content]; !ok || m.AuthorAddress != author || m.TrustOrigin != "external" || m.Unverified {
			f.t.Fatalf("%q on %s: %+v, want external from %s, verified", content, f.domain, m, author)
		}
	}

	step("lookup and directory", func(t *testing.T) {
		for f := range handles {
			for g, hs := range handles {
				if f == g {
					continue
				}
				for _, h := range hs {
					addr := h + "@" + g.domain
					r := f.admin("GET", "/v1/bots/lookup?handle="+addr, nil)
					var e struct {
						Handle, Address string
						Remote          bool
					}
					if r.Status != 200 || json.Unmarshal(r.Body, &e) != nil || !e.Remote || e.Handle != h || e.Address != addr {
						t.Fatalf("lookup of %s on %s: %d %s", addr, f.domain, r.Status, r.Body)
					}
					waitFor(t, addr+" in the directory of "+f.domain, func() bool {
						return strings.Contains(string(f.admin("GET", "/v1/directory", nil).Body), addr)
					})
				}
			}
		}
	})

	// want is proj's messages, in the order every server lists them.
	var want []string
	step("room on A with bots of B and C", func(t *testing.T) {
		r := a.admin("POST", "/v1/rooms", map[string]string{"name": "proj"})
		var room struct{ ID string }
		if r.Status != 201 || json.Unmarshal(r.Body, &room) != nil {
			t.Fatalf("create proj: %d %s", r.Status, r.Body)
		}
		proj = room.ID
		invites := []map[string]any{{"key_id": a1.ID}}
		for _, addr := range []string{b1Addr, b2Addr, c1Addr, c2Addr} {
			invites = append(invites, map[string]any{"address": addr})
		}
		for _, in := range invites {
			in["rights"] = []string{"all"}
			if r := a.admin("POST", "/v1/rooms/"+proj+"/members", in); r.Status != 201 {
				t.Fatalf("member %v: %d %s", in, r.Status, r.Body)
			}
		}

		// Messages of the three servers, in the same order on each.
		for _, m := range []struct {
			f       *fedEnv
			k       apiKey
			content string
		}{{a, a1, "a1: hello"}, {b, b1, "b1: hello"}, {c, c1, "c1: hello"}, {c, c2, "c2: hello"}, {b, b2, "b2: hello"}} {
			say(m.f, m.k, proj, m.content)
			want = append(want, m.content)
		}
		inOrder(t, want, all...)
		// The third server verifies the author signature: B's on C, C's on B.
		external(c, c1, proj, "b1: hello", b1Addr)
		external(b, b1, proj, "c1: hello", c1Addr)

		// Claims go to A: b1's claim conflicts for c1, and once b1
		// releases it, c1 holds the path, as B sees it.
		claim := func(f *fedEnv, k apiKey, sub string) httpResp {
			return f.callAs("POST", "/v1/rooms/"+proj+"/claims"+sub, k, map[string]any{"paths": []string{"src/main.go"}})
		}
		if r := claim(b, b1, ""); r.Status != 201 {
			t.Fatalf("b1 claims: %d %s", r.Status, r.Body)
		}
		r = claim(c, c1, "")
		var conflict struct {
			Conflicts []struct {
				Label       string
				TrustOrigin string `json:"trust_origin"`
			}
		}
		if r.Status != 409 || json.Unmarshal(r.Body, &conflict) != nil || len(conflict.Conflicts) != 1 ||
			conflict.Conflicts[0].Label != b1Addr || conflict.Conflicts[0].TrustOrigin != "external" {
			t.Fatalf("c1 claims the same path: %d %s", r.Status, r.Body)
		}
		if r := claim(b, b1, "/release"); r.Status != 200 || !strings.Contains(string(r.Body), "src/main.go") {
			t.Fatalf("b1 releases: %d %s", r.Status, r.Body)
		}
		if r := claim(c, c1, ""); r.Status != 201 {
			t.Fatalf("c1 claims after the release: %d %s", r.Status, r.Body)
		}
		if r := b.callAs("GET", "/v1/rooms/"+proj+"/claims", b1, nil); r.Status != 200 || !strings.Contains(string(r.Body), `"label":"`+c1Addr+`"`) {
			t.Fatalf("claims on B: %d %s", r.Status, r.Body)
		}

		// b2 leaves through B: A and C's replica lose it.
		waitFor(t, "b2 in C's replica", func() bool { return strings.Contains(members(c), b2Addr) })
		if r := b.admin("DELETE", "/v1/rooms/"+proj+"/members/"+strconv.FormatInt(b2.ID, 10), nil); r.Status != 204 {
			t.Fatalf("b2 leaves: %d %s", r.Status, r.Body)
		}
		if strings.Contains(members(a), b2Addr) {
			t.Fatalf("b2 still a member on A: %s", members(a))
		}
		waitFor(t, "b2 out of C's replica", func() bool { return !strings.Contains(members(c), b2Addr) })

		// A deletes c1's message once every server has it; each retracts it.
		say(c, c1, proj, "c1: deleted by A")
		inOrder(t, append(slices.Clone(want), "c1: deleted by A"), all...)
		id := a.messages(a1, proj)["c1: deleted by A"].ID
		if r := a.admin("DELETE", fmt.Sprintf("/v1/rooms/%s/messages/%d", proj, id), nil); r.Status != 204 {
			t.Fatalf("delete on A: %d %s", r.Status, r.Body)
		}
		inOrder(t, want, all...)
	})

	var dm string
	step("direct conversation of B and C", func(t *testing.T) {
		r := b.dmSend(b1, c1Addr, "b1 to c1")
		var out struct {
			RoomID string `json:"room_id"`
		}
		if r.Status != 201 || json.Unmarshal(r.Body, &out) != nil || out.RoomID == "" {
			t.Fatalf("b1 starts: %d %s", r.Status, r.Body)
		}
		dm = out.RoomID
		waitFor(t, "the DM on C", func() bool {
			cs := c.dmList(c1)
			return len(cs) == 1 && cs[0].RoomID == dm && cs[0].Address == b1Addr && cs[0].Unread == 1
		})
		external(c, c1, dm, "b1 to c1", b1Addr)
		if r := c.dmSend(c1, b1Addr, "c1 to b1"); r.Status != 201 || !strings.Contains(string(r.Body), dm) {
			t.Fatalf("c1 answers: %d %s", r.Status, r.Body)
		}
		waitFor(t, "the answer on B", func() bool {
			cs := b.dmList(b1)
			return len(cs) == 1 && cs[0].RoomID == dm && cs[0].Address == c1Addr && cs[0].Unread == 1
		})
		external(b, b1, dm, "c1 to b1", c1Addr)
	})

	step("bots of B and C follow a bot of A", func(t *testing.T) {
		policy := func(p string) {
			if r := a.admin("PATCH", "/v1/bots/a1/profile", map[string]string{"follow_policy": p}); r.Status != 200 {
				t.Fatalf("a1's policy %s: %d %s", p, r.Status, r.Body)
			}
		}
		followed := func(f *fedEnv, k apiKey) bool {
			fs := f.follows(k, "/v1/follows", "follows")
			return len(fs) == 1 && fs[0].To == a1Addr && fs[0].Status == "accepted"
		}
		// Open: b1's follow is accepted at once.
		policy("open")
		if r := b.callAs("POST", "/v1/follows", b1, map[string]string{"to": a1Addr}); r.Status != 201 {
			t.Fatalf("b1 follows a1: %d %s", r.Status, r.Body)
		}
		waitFor(t, "b1's follow accepted", func() bool { return followed(b, b1) })
		// Approve: c1's follow waits until a1 accepts it.
		policy("approve")
		r := c.callAs("POST", "/v1/follows", c1, map[string]string{"to": a1Addr})
		if r.Status != 201 || !strings.Contains(string(r.Body), `"status":"pending"`) {
			t.Fatalf("c1 follows a1: %d %s", r.Status, r.Body)
		}
		reqs := a.follows(a1, "/v1/follows/requests", "requests")
		if len(reqs) != 1 || reqs[0].From != c1Addr || reqs[0].Status != "pending" {
			t.Fatalf("requests on A: %+v", reqs)
		}
		if r := a.callAs("POST", "/v1/follows/requests/"+strconv.FormatInt(reqs[0].ID, 10)+"/accept", a1, nil); r.Status != 200 {
			t.Fatalf("a1 accepts: %d %s", r.Status, r.Body)
		}
		waitFor(t, "c1's follow accepted", func() bool { return followed(c, c1) })
		if fs := a.follows(a1, "/v1/followers", "followers"); len(fs) != 2 {
			t.Fatalf("a1's followers: %+v", fs)
		}

		// a1 posts once; each follower's timeline has it once.
		a.post(a1, "a1 posts once", "followers")
		for _, s := range []reader{{b, b1}, {c, c1}} {
			var p tlPost
			waitFor(t, "the post on "+s.f.domain, func() bool { var ok bool; p, ok = s.f.timeline(s.k)["a1 posts once"]; return ok })
			if p.AuthorAddress != a1Addr || p.TrustOrigin != "external" || p.Unverified {
				t.Fatalf("post on %s: %+v", s.f.domain, p)
			}
			r := s.f.callAs("GET", "/v1/timeline?limit=100", s.k, nil)
			if n := strings.Count(string(r.Body), `"content":"a1 posts once"`); n != 1 {
				t.Fatalf("the post %d times on %s: %s", n, s.f.domain, r.Body)
			}
		}
	})

	step("C down and catching up", func(t *testing.T) {
		c.close()
		say(a, a1, proj, "a1: while C was down")
		say(b, b1, proj, "b1: while C was down")
		want = append(want, "a1: while C was down", "b1: while C was down")
		waitFor(t, "C failing on A", func() bool { return a.peerView("GET", c.domain, "").Health == "failing" })
		inOrder(t, want, reader{a, a1}, reader{b, b1})
		c.t = root // the restarted server outlives this step
		c.start()
		c.t = t
		inOrder(t, want, all...)
		external(c, c1, proj, "b1: while C was down", b1Addr)
	})

	step("B rotates its key", func(t *testing.T) {
		oldFP := b.fingerprint()
		r := b.admin("POST", "/v1/federation/keys/rotate", nil)
		var rot struct{ Fingerprint string }
		if r.Status != 200 || json.Unmarshal(r.Body, &rot) != nil || rot.Fingerprint == oldFP {
			t.Fatalf("rotate: %d %s", r.Status, r.Body)
		}
		// A follows the rotation on b1's message, C on its author signature.
		say(b, b1, proj, "b1: after the rotation")
		want = append(want, "b1: after the rotation")
		inOrder(t, want, all...)
		external(a, a1, proj, "b1: after the rotation", b1Addr)
		external(c, c1, proj, "b1: after the rotation", b1Addr)
		if r := b.dmSend(b1, c1Addr, "b1 after the rotation"); r.Status != 201 || !strings.Contains(string(r.Body), dm) {
			t.Fatalf("b1's DM: %d %s", r.Status, r.Body)
		}
		waitFor(t, "the DM on C", func() bool { _, ok := c.messages(c1, dm)["b1 after the rotation"]; return ok })
		external(c, c1, dm, "b1 after the rotation", b1Addr)
		for _, f := range []*fedEnv{a, c} {
			if v := f.peerView("GET", b.domain, ""); v.Fingerprint != rot.Fingerprint || v.Status != "allowed" {
				t.Fatalf("%s's view of B: %+v, want %s", f.domain, v, rot.Fingerprint)
			}
			if n, m := f.auditCount("federation.key_rotated"), f.auditCount("federation.key_changed"); n != 1 || m != 0 {
				t.Fatalf("%s audited %d key_rotated, %d key_changed; want 1, 0", f.domain, n, m)
			}
		}
	})

	step("A blocks a bot of C, then blocks and purges C", func(t *testing.T) {
		r := a.admin("POST", "/v1/federation/blocked-bots", map[string]string{"address": c2Addr, "note": "e2e"})
		if r.Status != 201 {
			t.Fatalf("block c2: %d %s", r.Status, r.Body)
		}
		if strings.Contains(members(a), c2Addr) {
			t.Fatalf("c2 still a member on A: %s", members(a))
		}
		waitFor(t, "c2 out of B's replica", func() bool { return !strings.Contains(members(b), c2Addr) })

		if v := a.peerView("POST", c.domain, "/block"); v.Status != "blocked" {
			t.Fatalf("block C: %+v", v)
		}
		if fs := a.follows(a1, "/v1/followers", "followers"); len(fs) != 1 || fs[0].From != b1Addr {
			t.Fatalf("a1's followers after the block: %+v", fs)
		}
		// The purge counts what the blocks left: c1's and c2's messages in
		// proj, the cursors of proj and a1's feed, and c1's stand-in
		// (c2's is revoked already). The block ended the memberships,
		// c1's claim and its follow of a1.
		r = a.admin("POST", "/v1/federation/peers/"+c.domain+"/purge", nil)
		if r.Status != 200 || !strings.Contains(string(r.Body),
			`{"rooms":0,"messages":2,"members":0,"follows":0,"claims":0,"cursors":2,"stand_ins":1}`) {
			t.Fatalf("purge C: %d %s", r.Status, r.Body)
		}
		// B's replica retracts what C's bots wrote, as A does.
		var kept []string
		for _, s := range want {
			if !strings.HasPrefix(s, "c1:") && !strings.HasPrefix(s, "c2:") {
				kept = append(kept, s)
			}
		}
		inOrder(t, kept, reader{a, a1}, reader{b, b1})
		waitFor(t, "c1 out of B's replica", func() bool { return !strings.Contains(members(b), c1Addr) })
	})
}
