package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fedGet is an unsigned GET of a public page on f's federation listener.
func (f *fedEnv) fedGet(path string) (int, http.Header, string) {
	f.t.Helper()
	resp, err := http.Get("http://" + f.domain + path)
	if err != nil {
		f.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// botKey creates a non-admin key on f and gives it handle.
func (f *fedEnv) botKey(handle string) apiKey {
	f.t.Helper()
	k := f.createKey(handle, false)
	r := f.admin("PUT", "/v1/keys/"+strconv.FormatInt(k.ID, 10)+"/handle", map[string]string{"handle": handle})
	if r.Status != 200 {
		f.t.Fatalf("set handle %s: %d %s", handle, r.Status, r.Body)
	}
	return k
}

func TestBots_AcrossPeers(t *testing.T) {
	a, b := newFedEnv(t), newFedEnv(t)

	// Three bots on B, one per visibility.
	keys := map[string]apiKey{}
	for handle, vis := range map[string]string{"pbot": "peers", "hbot": "private", "wbot": "public"} {
		keys[handle] = b.botKey(handle)
		patch := map[string]any{"visibility": vis, "description": "the " + vis + " bot"}
		if handle == "wbot" {
			patch["display_name"] = "<b>W & Bot</b>"
		}
		if r := b.admin("PATCH", "/v1/bots/"+handle+"/profile", patch); r.Status != 200 {
			t.Fatalf("patch %s: %d %s", handle, r.Status, r.Body)
		}
	}

	// B admits A first, so A's directory refresh on adding B is answered.
	// ponytail: the refresh runs in Run right after A adds B; the test
	// polls for it instead of a test-only hook.
	b.peer(a)
	a.peer(b)

	// Lookup from A: a peers bot is found, a private one is not.
	r := a.admin("GET", "/v1/bots/lookup?handle=pbot@"+b.domain, nil)
	var e struct {
		Handle, Address string
		Remote          bool
	}
	if r.Status != 200 || json.Unmarshal(r.Body, &e) != nil || !e.Remote || e.Handle != "pbot" || e.Address != "pbot@"+b.domain {
		t.Fatalf("lookup pbot: %d %s", r.Status, r.Body)
	}
	if r := a.admin("GET", "/v1/bots/lookup?handle=hbot@"+b.domain, nil); r.Status != 404 {
		t.Fatalf("lookup hbot: %d %s", r.Status, r.Body)
	}

	// The directory on A fills after the refresh.
	var dir string
	for i := 0; i < 200; i++ {
		dir = string(a.admin("GET", "/v1/directory", nil).Body)
		if strings.Contains(dir, "pbot@"+b.domain) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !strings.Contains(dir, "pbot@"+b.domain) || !strings.Contains(dir, "wbot@"+b.domain) || strings.Contains(dir, "hbot") {
		t.Fatalf("directory: %s", dir)
	}

	// B's public pages.
	status, hdr, body := b.fedGet("/@wbot")
	if status != 200 || !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") ||
		!strings.Contains(body, "&lt;b&gt;W &amp; Bot&lt;/b&gt;") || strings.Contains(body, "<b>W") {
		t.Fatalf("/@wbot: %d %v %s", status, hdr, body)
	}
	for _, p := range []string{"/@pbot", "/@hbot", "/@nobody"} {
		if status, _, _ := b.fedGet(p); status != 404 {
			t.Fatalf("%s: %d, want 404", p, status)
		}
	}
	status, _, body = b.fedGet("/@wbot/agent-card.json")
	var card struct{ Name, Description string }
	if status != 200 || json.Unmarshal([]byte(body), &card) != nil || card.Name != "<b>W & Bot</b>" || card.Description != "the public bot" {
		t.Fatalf("agent card: %d %s", status, body)
	}

	// A bot edits its own description, but not its visibility.
	w := keys["wbot"]
	if r := b.callAs("PATCH", "/v1/bots/wbot/profile", w, map[string]any{"description": "new text"}); r.Status != 200 {
		t.Fatalf("self description: %d %s", r.Status, r.Body)
	}
	if r := b.callAs("PATCH", "/v1/bots/wbot/profile", w, map[string]any{"visibility": "private"}); r.Status != 403 {
		t.Fatalf("self visibility: %d %s", r.Status, r.Body)
	}
	if r := b.callAs("PATCH", "/v1/bots/pbot/profile", w, map[string]any{"description": "x"}); r.Status != 403 {
		t.Fatalf("edit another bot: %d %s", r.Status, r.Body)
	}
}
