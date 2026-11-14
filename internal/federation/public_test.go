package federation

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	for _, path := range []string{"/", "/directory", "/@pub", "/@pub/agent-card.json", "/@pub/feed.xml", "/public.css",
		"/@priv", "/@peersbot", "/@nobody", "/@priv/agent-card.json", "/@nobody/agent-card.json", "/@priv/feed.xml",
		"/@nobody/feed.xml", "/@bad%20name"} {
		rec := pubGet(svc, path)
		h := rec.Header()
		if h.Get("Content-Security-Policy") != wantCSP || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Referrer-Policy") != "no-referrer" || h.Get("Set-Cookie") != "" {
			t.Errorf("%s: headers %v", path, h)
		}
	}
	// Router-level 404 and 405 answers carry them too.
	for _, req := range []*http.Request{httptest.NewRequest(http.MethodGet, "/@x/", nil),
		httptest.NewRequest(http.MethodGet, "/directory/", nil), httptest.NewRequest(http.MethodHead, "/", nil),
		httptest.NewRequest(http.MethodPost, "/directory", nil)} {
		rec := serve(svc, req)
		h := rec.Header()
		if rec.Code < 400 || h.Get("Content-Security-Policy") != wantCSP || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: %d, headers %v", req.Method, req.URL, rec.Code, h)
		}
	}
	// Pages are revalidated, so a bot made private leaves caches.
	for _, path := range []string{"/directory", "/@pub", "/@nobody", "/@pub/agent-card.json", "/@pub/feed.xml", "/@priv/feed.xml"} {
		if got := pubGet(svc, path).Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control %q", path, got)
		}
	}
	want := map[string]string{
		"/directory": "text/html; charset=utf-8", "/@pub": "text/html; charset=utf-8", "/@nobody": "text/html; charset=utf-8",
		"/@pub/agent-card.json": "application/json", "/public.css": "text/css; charset=utf-8",
		"/@pub/feed.xml": "application/atom+xml; charset=utf-8", "/@priv/feed.xml": "text/html; charset=utf-8",
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
	for _, path := range []string{"/@priv", "/@peersbot", "/@Bad%20Name", "/@priv/agent-card.json", "/@nobody/agent-card.json",
		"/@peersbot/agent-card.json", "/@priv/feed.xml", "/@peersbot/feed.xml", "/@nobody/feed.xml", "/@Bad%20Name/feed.xml"} {
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

// addFeed makes a feed of handle on local, archived or active, and
// returns its id.
func addFeed(t *testing.T, svc *Service, handle string, archived bool) string {
	t.Helper()
	id := "feed-" + handle + fmt.Sprint(archived)
	var arch any
	if archived {
		arch = "2026-01-02T00:00:00Z"
	}
	if _, err := svc.db.Exec(`INSERT INTO rooms(id, name, owner_key_id, created_at, visibility, feed, archived_at)
		VALUES (?, ?, (SELECT id FROM api_keys WHERE handle = ?), '2026-01-01T00:00:00Z', 'private', ?, ?)`,
		id, id, handle, handle+"@"+local, arch); err != nil {
		t.Fatal(err)
	}
	return id
}

// addPost stores a message by author in room at minute min of a day,
// with metadata meta ("" for none), deleted or not.
func addPost(t *testing.T, svc *Service, room, author string, min int, content, meta string, deleted bool) int64 {
	t.Helper()
	var m, del any
	if meta != "" {
		m = meta
	}
	if deleted {
		del = "2026-01-03T00:00:00Z"
	}
	res, err := svc.db.Exec(`INSERT INTO messages(room_id, author_key_id, ts, content, content_type, message_type, metadata, deleted_at)
		VALUES (?, (SELECT id FROM api_keys WHERE handle = ?), ?, ?, 'text/plain', 'status_update', ?, ?)`,
		room, author, fmt.Sprintf("2026-01-01T%02d:%02d:00.000000000Z", min/60, min%60), content, m, del)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

const pubMeta = `{"visibility":"public"}`

func atom(t *testing.T, svc *Service, path string) atomFeed {
	t.Helper()
	rec := pubGet(svc, path)
	var f atomFeed
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: %d", path, rec.Code)
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatalf("%s does not parse: %v\n%s", path, err, rec.Body)
	}
	return f
}

// Only public, non-deleted posts by the bot in its active feed show,
// on the page and in the feed, escaped and newest first.
func TestPublic_Posts(t *testing.T) {
	svc := publicSvc(t)
	addBot(t, svc.db, "other", bots.Public)
	feed := addFeed(t, svc, "pub", false)
	old := addFeed(t, svc, "pub", true)
	privFeed := addFeed(t, svc, "priv", false)
	const evil = "<script>alert(3)</script> \"q\" 'a' </p></article><!-- -->\nsecond line"
	first := addPost(t, svc, feed, "pub", 1, "first public", pubMeta, false)
	addPost(t, svc, feed, "pub", 2, "secret followers", `{"visibility":"followers"}`, false)
	addPost(t, svc, feed, "pub", 3, "no metadata", "", false)
	addPost(t, svc, feed, "pub", 4, "broken metadata", `{"visibility":"public"`, false)
	addPost(t, svc, feed, "pub", 5, "deleted public", pubMeta, true)
	addPost(t, svc, feed, "other", 6, "not by the bot", pubMeta, false)
	addPost(t, svc, old, "pub", 7, "archived public", pubMeta, false)
	addPost(t, svc, privFeed, "priv", 8, "private bot post", pubMeta, false)
	hostile := addPost(t, svc, feed, "pub", 9, evil, pubMeta, false)

	page := pubGet(svc, "/@pub").Body.String()
	for _, absent := range []string{"secret followers", "no metadata", "broken metadata", "deleted public", "not by the bot",
		"archived public", "private bot post", "<script>alert(3)", "</p></article><!--"} {
		if strings.Contains(page, absent) {
			t.Errorf("page shows %q:\n%s", absent, page)
		}
	}
	i, j := strings.Index(page, "&lt;script&gt;alert(3)&lt;/script&gt; &#34;q&#34; &#39;a&#39; &lt;/p&gt;&lt;/article&gt;&lt;!-- --&gt;\nsecond line"),
		strings.Index(page, "first public")
	if i < 0 || j < 0 || i > j {
		t.Errorf("posts missing or out of order (%d, %d):\n%s", i, j, page)
	}
	for _, want := range []string{`href="/@pub/feed.xml"`, `<link rel="alternate" type="application/atom+xml"`, `datetime="2026-01-01T00:09:00Z"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s:\n%s", want, page)
		}
	}
	if b := pubGet(svc, "/@priv/feed.xml").Body.String(); strings.Contains(b, "private bot post") {
		t.Errorf("private feed: %s", b)
	}

	f := atom(t, svc, "/@pub/feed.xml")
	p, _ := bots.NewStore(svc.db).Get(context.Background(), "pub")
	if f.ID != "tag:"+local+",2026:feed/pub" || f.Title != p.DisplayName || f.Updated != "2026-01-01T00:09:00Z" || f.Author.Name != p.DisplayName {
		t.Errorf("feed head: %+v", f)
	}
	links := map[string]string{}
	for _, l := range f.Links {
		links[l.Rel] = l.Type + " " + l.Href
	}
	if links["self"] != "application/atom+xml https://"+local+"/@pub/feed.xml" || links["alternate"] != "text/html https://"+local+"/@pub" {
		t.Errorf("links: %v", links)
	}
	if len(f.Entries) != 2 {
		t.Fatalf("entries: %+v", f.Entries)
	}
	e := f.Entries[0]
	if e.ID != fmt.Sprintf("tag:%s,2026:post/%d", local, hostile) || e.Updated != "2026-01-01T00:09:00Z" ||
		e.Title != "<script>alert(3)</script> \"q\" 'a' </p></article><!-- -->" || e.Content.Type != "text" || e.Content.Text != evil {
		t.Errorf("entry 0: %+v", e)
	}
	if e := f.Entries[1]; e.ID != fmt.Sprintf("tag:%s,2026:post/%d", local, first) || e.Content.Text != "first public" {
		t.Errorf("entry 1: %+v", e)
	}
}

// A bot without posts has an empty feed dated by its profile.
func TestPublic_FeedWithoutPosts(t *testing.T) {
	svc := publicSvc(t)
	f := atom(t, svc, "/@pub/feed.xml")
	p, _ := bots.NewStore(svc.db).Get(context.Background(), "pub")
	if len(f.Entries) != 0 || f.Updated != p.UpdatedAt.UTC().Format(time.RFC3339) {
		t.Errorf("feed: %+v, profile updated %v", f, p.UpdatedAt)
	}
	if page := pubGet(svc, "/@pub").Body.String(); !strings.Contains(page, "No public posts.") {
		t.Errorf("page:\n%s", page)
	}
}

// Markup, CDATA ends, ampersands and control characters cannot break
// the feed: it parses, and forbidden characters are gone.
func TestPublic_FeedEscaping(t *testing.T) {
	svc := publicSvc(t)
	feed := addFeed(t, svc, "pub", false)
	long := strings.Repeat("é", 100)
	content := long + "\n]]> & <x/> </content></entry></feed> \"q\" 'a' \ttab\r\nend \x01\x08\x0b\x0c\x1b\x1f\ufffe\uffff\x00"
	addPost(t, svc, feed, "pub", 1, content, pubMeta, false)
	rec := pubGet(svc, "/@pub/feed.xml")
	body := rec.Body.String()
	if strings.Contains(body, "]]>") || strings.Contains(body, "<x/>") || strings.Contains(body, "</feed> ") || strings.Count(body, "<entry>") != 1 {
		t.Errorf("unescaped content:\n%s", body)
	}
	for _, r := range body {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0xFFFE || r == 0xFFFF {
			t.Errorf("forbidden character %U in feed", r)
		}
	}
	f := atom(t, svc, "/@pub/feed.xml")
	if len(f.Entries) != 1 {
		t.Fatalf("entries: %+v", f.Entries)
	}
	e := f.Entries[0]
	if e.Title != strings.Repeat("é", 80) {
		t.Errorf("title %q", e.Title)
	}
	if !strings.Contains(e.Content.Text, "]]> & <x/> </content></entry></feed> \"q\" 'a' ") || !strings.Contains(e.Content.Text, "\ttab\r\nend") {
		t.Errorf("content %q", e.Content.Text)
	}
}

// The page and the feed show the newest 20 posts, newest first.
func TestPublic_PostsCapAndOrder(t *testing.T) {
	svc := publicSvc(t)
	feed := addFeed(t, svc, "pub", false)
	for i := range 25 {
		addPost(t, svc, feed, "pub", i, fmt.Sprintf("post-%02d", i), pubMeta, false)
	}
	page := pubGet(svc, "/@pub").Body.String()
	f := atom(t, svc, "/@pub/feed.xml")
	if len(f.Entries) != 20 {
		t.Fatalf("feed has %d entries", len(f.Entries))
	}
	last := -1
	for i := 24; i >= 0; i-- {
		text := fmt.Sprintf("post-%02d", i)
		at := strings.Index(page, text)
		if i < 5 {
			if at >= 0 {
				t.Errorf("page shows %s", text)
			}
			continue
		}
		if at <= last {
			t.Errorf("page: %s missing or out of order", text)
		}
		last = at
		if got := f.Entries[24-i].Content.Text; got != text {
			t.Errorf("entry %d: %q, want %q", 24-i, got, text)
		}
	}
}
