package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// rightsEnv: an admin key (keyA) creates room R and invites one agent
// key per right under test, holding only that right, and keyB without
// rights. outsider is in no room; admin2 is an admin key in no room.
type rightsEnv struct {
	*testEnv
	room           string
	none, outsider apiKey
	admin2         apiKey
	holder         map[string]apiKey
}

func newRightsEnv(t *testing.T) *rightsEnv {
	t.Helper()
	env := newTestEnv(t)
	e := &rightsEnv{testEnv: env, holder: map[string]apiKey{}}
	create := env.callAs("POST", "/v1/rooms", env.keyA, map[string]any{"name": "rights-r"})
	if create.Status != 201 {
		t.Fatalf("create room: %d %s", create.Status, create.Body)
	}
	e.room = mustField(t, create.Body, "id")
	invite := func(k apiKey, rights []string) {
		t.Helper()
		resp := env.callAs("POST", "/v1/rooms/"+e.room+"/invite", env.keyA, map[string]any{"key_id": k.ID, "rights": rights})
		if resp.Status != 201 {
			t.Fatalf("invite key %d: %d %s", k.ID, resp.Status, resp.Body)
		}
	}
	for _, right := range []string{"read", "claim", "handoff", "system"} {
		k := env.createKey("holder-"+right, false)
		invite(k, []string{right})
		e.holder[right] = k
	}
	e.none = env.keyB
	invite(e.none, []string{})
	e.outsider = env.createKey("outsider", false)
	e.admin2 = env.createKey("admin-2", true)
	return e
}

// TestRights_Matrix: every endpoint of spec section 3 against a
// non-member (404), a member without the right (403 missing_right),
// a member with it (2xx) and an admin key that is not a member (2xx).
func TestRights_Matrix(t *testing.T) {
	e := newRightsEnv(t)
	base := "/v1/rooms/" + e.room
	empty := func(apiKey) any { return map[string]any{} }
	for _, c := range []struct {
		method, path, right string
		body                func(apiKey) any
	}{
		{"GET", base + "/messages", "read", nil},
		{"GET", base + "/events?wait=1", "read", nil},
		{"POST", base + "/messages", "handoff", func(apiKey) any { return map[string]any{"content": "x", "message_type": "handoff"} }},
		{"POST", base + "/messages", "system", func(apiKey) any { return map[string]any{"content": "x"} }},
		{"GET", base + "/claims", "read", nil},
		{"POST", base + "/claims", "claim", func(k apiKey) any { return map[string]any{"paths": []string{fmt.Sprintf("k%d.go", k.ID)}} }},
		{"POST", base + "/claims/heartbeat", "claim", empty},
		{"POST", base + "/claims/release", "claim", empty},
		{"GET", base + "/members", "read", nil},
		{"GET", base, "", nil}, // any membership, even without rights
	} {
		call := func(k apiKey) httpResp {
			var body any
			if c.body != nil {
				body = c.body(k)
			}
			return e.callAs(c.method, c.path, k, body)
		}
		name := c.method + " " + c.path
		if r := call(e.outsider); r.Status != 404 {
			t.Errorf("%s as non-member: %d %s, want 404", name, r.Status, r.Body)
		}
		with := e.none
		if c.right != "" {
			with = e.holder[c.right]
			r := call(e.none)
			if r.Status != 403 || !strings.Contains(string(r.Body), `"code":"missing_right"`) || !strings.Contains(string(r.Body), "missing right: "+c.right) {
				t.Errorf("%s without %s: %d %s, want 403 missing right: %s", name, c.right, r.Status, r.Body, c.right)
			}
		}
		if r := call(with); r.Status/100 != 2 {
			t.Errorf("%s as member with %q: %d %s, want 2xx", name, c.right, r.Status, r.Body)
		}
		if r := call(e.admin2); r.Status/100 != 2 {
			t.Errorf("%s as admin key (not a member): %d %s, want 2xx", name, r.Status, r.Body)
		}
	}
}

// TestRights_DiscoveryForAgentKeys: an agent key sees its own room and
// nothing else: no key list, no audit log, no other room.
func TestRights_DiscoveryForAgentKeys(t *testing.T) {
	e := newRightsEnv(t)
	if r := e.callAs("POST", "/v1/rooms", e.keyA, map[string]any{"name": "rights-public", "visibility": "public"}); r.Status != 201 {
		t.Fatalf("public room: %d %s", r.Status, r.Body)
	}
	agent := e.holder["read"]
	for _, p := range []string{"/v1/keys", "/v1/audit"} {
		if r := e.callAs("GET", p, agent, nil); r.Status != 403 || !strings.Contains(string(r.Body), "admin key required") {
			t.Errorf("GET %s as agent: %d %s, want 403 admin key required", p, r.Status, r.Body)
		}
	}
	list := e.callAs("GET", "/v1/rooms", agent, nil)
	var rooms struct{ Rooms []struct{ ID, Name string } }
	_ = json.Unmarshal(list.Body, &rooms)
	if list.Status != 200 || len(rooms.Rooms) != 1 || rooms.Rooms[0].ID != e.room {
		t.Errorf("agent's rooms: %d %s, want only %s", list.Status, list.Body, e.room)
	}
	if r := e.callAs("GET", "/v1/rooms?visibility=public", agent, nil); strings.Contains(string(r.Body), "rights-public") {
		t.Errorf("a public room the agent is not in is listed: %s", r.Body)
	}
	if r := e.callAs("GET", "/v1/rooms", e.admin2, nil); !strings.Contains(string(r.Body), "rights-public") || !strings.Contains(string(r.Body), e.room) {
		t.Errorf("an admin key lists every room: %s", r.Body)
	}
}

// TestRights_ManagementIsAdminOnly: every admin-only endpoint answers
// 403 to a key that is not an admin key.
func TestRights_ManagementIsAdminOnly(t *testing.T) {
	e := newRightsEnv(t)
	agent := e.holder["read"]
	base := "/v1/rooms/" + e.room
	member := fmt.Sprintf("%s/members/%d", base, e.none.ID)
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/v1/rooms", map[string]any{"name": "nope"}},
		{"POST", base + "/archive", nil},
		{"POST", base + "/invite", map[string]any{"key_id": e.outsider.ID, "rights": []string{"read"}}},
		{"POST", base + "/members", map[string]any{"key_id": e.outsider.ID, "rights": []string{"read"}}},
		{"PATCH", member, map[string]any{"rights": []string{"read"}}},
		{"DELETE", member, nil},
		{"DELETE", base + "/messages/1", nil},
		{"POST", "/v1/keys", map[string]any{"label": "nope"}},
		{"DELETE", fmt.Sprintf("/v1/keys/%d", e.none.ID), nil},
	} {
		if r := e.callAs(c.method, c.path, agent, c.body); r.Status != 403 || !strings.Contains(string(r.Body), "admin key required") {
			t.Errorf("%s %s as agent: %d %s, want 403 admin key required", c.method, c.path, r.Status, r.Body)
		}
	}
}
