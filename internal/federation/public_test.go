package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hermes/conclave-mcp/internal/bots"
)

const wantCSP = "default-src 'none'; style-src 'self'; img-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func pubGet(svc *Service, path string) *httptest.ResponseRecorder {
	return serve(svc, httptest.NewRequest(http.MethodGet, path, nil))
}

// publicSvc has a public bot with every field filled with markup, a
// peers bot and a private bot.
func publicSvc(t *testing.T) *Service {
	t.Helper()
	svc := newLocal(t, "allowlist")
	addBot(t, svc.db, "pub", bots.Public)
	addBot(t, svc.db, "peersbot", bots.Peers)
	addBot(t, svc.db, "priv", bots.Private)
	const evil = `<script>alert(1)</script><img src=x onerror=alert(2)> & "q" 'a'`
	name, desc := evil, "line one\n"+evil
	sk := []bots.Skill{{ID: "s1", Name: evil, Description: evil}}
	if _, err := bots.NewStore(svc.db).Update(context.Background(), "pub",
		bots.Patch{DisplayName: &name, Description: &desc, Skills: &sk}, true); err != nil {
		t.Fatal(err)
	}
	// A skill id cannot hold markup through the API; a row written by
	// other means still comes out escaped.
	if _, err := svc.db.Exec(`UPDATE bot_profiles SET skills = ? WHERE key_id = (SELECT id FROM api_keys WHERE handle = 'pub')`,
		`[{"id":"<b>id</b>","name":"`+strings.ReplaceAll(evil, `"`, `\"`)+`","description":"d"}]`); err != nil {
		t.Fatal(err)
	}
	return svc
}

// Every response, errors and the redirect included, carries the
// headers and sets no cookie.
func TestPublic_Headers(t *testing.T) {
	svc := publicSvc(t)
	for _, path := range []string{"/", "/directory", "/@pub", "/@pub/agent-card.json", "/public.css",
		"/@priv", "/@peersbot", "/@nobody", "/@priv/agent-card.json", "/@nobody/agent-card.json", "/@bad%20name"} {
		rec := pubGet(svc, path)
		h := rec.Header()
		if h.Get("Content-Security-Policy") != wantCSP || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Referrer-Policy") != "no-referrer" || h.Get("Set-Cookie") != "" {
			t.Errorf("%s: headers %v", path, h)
		}
	}
	want := map[string]string{
		"/directory": "text/html; charset=utf-8", "/@pub": "text/html; charset=utf-8", "/@nobody": "text/html; charset=utf-8",
		"/@pub/agent-card.json": "application/json", "/public.css": "text/css; charset=utf-8",
	}
	for path, ct := range want {
		if got := pubGet(svc, path).Header().Get("Content-Type"); got != ct {
			t.Errorf("%s: content type %q, want %q", path, got, ct)
		}
	}
}

func TestPublic_RootRedirects(t *testing.T) {
	rec := pubGet(publicSvc(t), "/")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/directory" {
		t.Errorf("/: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// Markup in any field comes out escaped, and no output has a script.
func TestPublic_EscapesAndNoScripts(t *testing.T) {
	svc := publicSvc(t)
	for _, path := range []string{"/directory", "/@pub", "/@pub/agent-card.json", "/public.css", "/@nobody"} {
		body := pubGet(svc, path).Body.String()
		low := strings.ToLower(body)
		if strings.Contains(low, "<script") || strings.Contains(low, "<img") || strings.Contains(low, "<b>") ||
			strings.Contains(low, "javascript:") ||
			(!strings.HasSuffix(path, ".json") && (strings.Contains(low, "http://") || strings.Contains(low, "https://"))) {
			t.Errorf("%s: unescaped markup or external reference:\n%s", path, body)
		}
	}
	page := pubGet(svc, "/@pub").Body.String()
	for _, want := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&lt;b&gt;id&lt;/b&gt;", "pub@" + local, "line one"} {
		if !strings.Contains(page, want) {
			t.Errorf("bot page lacks %q:\n%s", want, page)
		}
	}
	if dir := pubGet(svc, "/directory").Body.String(); !strings.Contains(dir, "&lt;script&gt;") || !strings.Contains(dir, "pub@"+local) {
		t.Errorf("directory:\n%s", dir)
	}
}

// Private, peers-only, unknown and malformed names answer the same
// 404, byte for byte, for the page and the card.
func TestPublic_NotFoundIsIdentical(t *testing.T) {
	svc := publicSvc(t)
	ref := pubGet(svc, "/@nobody")
	if ref.Code != http.StatusNotFound || ref.Body.Len() == 0 {
		t.Fatalf("unknown: %d", ref.Code)
	}
	for _, path := range []string{"/@priv", "/@peersbot", "/@Bad%20Name", "/@priv/agent-card.json", "/@nobody/agent-card.json", "/@peersbot/agent-card.json"} {
		rec := pubGet(svc, path)
		if rec.Code != http.StatusNotFound || rec.Body.String() != ref.Body.String() || rec.Header().Get("Content-Type") != ref.Header().Get("Content-Type") {
			t.Errorf("%s: %d %q differs from the unknown name", path, rec.Code, rec.Body)
		}
	}
}

func TestPublic_DirectoryListsOnlyPublic(t *testing.T) {
	dir := pubGet(publicSvc(t), "/directory").Body.String()
	if !strings.Contains(dir, "/@pub") || strings.Contains(dir, "peersbot") || strings.Contains(dir, "priv@") {
		t.Errorf("directory:\n%s", dir)
	}
}

func TestPublic_AgentCardShape(t *testing.T) {
	svc := publicSvc(t)
	rec := pubGet(svc, "/@pub/agent-card.json")
	var card map[string]any
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &card) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	p, _ := bots.NewStore(svc.db).Get(context.Background(), "pub")
	want := map[string]any{
		"name": p.DisplayName, "description": p.Description, "version": p.UpdatedAt.Format("2006-01-02"),
		"skills":             []any{map[string]any{"id": "<b>id</b>", "name": p.Skills[0].Name, "description": "d"}},
		"defaultInputModes":  []any{"text/plain"},
		"defaultOutputModes": []any{"text/plain"},
		"capabilities":       map[string]any{},
		"supportedInterfaces": []any{map[string]any{
			"url": "https://" + local + "/@pub", "protocolBinding": "conclave-federation/1", "protocolVersion": "1"}},
		"x-conclave": map[string]any{"address": "pub@" + local},
	}
	got, _ := json.Marshal(card)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Errorf("card:\n got %s\nwant %s", got, exp)
	}
	// A bot without a display name or skills: the handle names it and skills is [].
	addBot(t, svc.db, "plain", bots.Public)
	if _, err := svc.db.Exec(`UPDATE bot_profiles SET display_name = '', skills = '[]' WHERE key_id = (SELECT id FROM api_keys WHERE handle = 'plain')`); err != nil {
		t.Fatal(err)
	}
	body := pubGet(svc, "/@plain/agent-card.json").Body.String()
	if !strings.Contains(body, `"name":"plain"`) || !strings.Contains(body, `"skills":[]`) {
		t.Errorf("plain card: %s", body)
	}
}

func TestPublic_ListsAtMost1000(t *testing.T) {
	svc := newLocal(t, "allowlist")
	for i := range 1001 {
		addBot(t, svc.db, fmt.Sprintf("b%04d", i), bots.Public)
	}
	body := pubGet(svc, "/directory").Body.String()
	if n := strings.Count(body, `href="/@b`); n != 1000 {
		t.Errorf("listed %d bots, want 1000", n)
	}
}

func TestPublic_Stylesheet(t *testing.T) {
	css := pubGet(publicSvc(t), "/public.css").Body.String()
	if !strings.Contains(css, "prefers-color-scheme: dark") || strings.Contains(css, "url(") || strings.Contains(css, "@import") {
		t.Errorf("stylesheet:\n%s", css)
	}
}
