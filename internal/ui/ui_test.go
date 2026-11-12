package ui

import (
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestPages_ServedWithSecurityHeaders(t *testing.T) {
	_, _, base := newTestUI(t)
	b := newBrowser(t, base)
	for path, ctype := range map[string]string{"/ui/": "text/html", "/ui/app.css": "text/css"} {
		resp := b.send("GET", path, nil, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) {
			t.Errorf("%s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		for k, want := range map[string]string{
			"Content-Security-Policy": "default-src 'self'; base-uri 'none'; frame-ancestors 'none'",
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
		} {
			if got := resp.Header.Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", path, k, got, want)
			}
		}
		if path == "/ui/" && !strings.Contains(string(body), `<script type="module" src="app.js"></script>`) {
			t.Errorf("index.html does not load app.js:\n%s", body)
		}
	}
	resp := b.send("GET", "/ui", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Errorf("/ui: %d to %q, want 302 to /ui/", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestPages_HostAndOriginChecks(t *testing.T) {
	_, _, base := newTestUI(t)
	b := newBrowser(t, base)
	b.host = "127.0.0.1:7432"
	resp := b.send("GET", "/ui/?x=1", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "http://localhost:7432/ui/?x=1" {
		t.Errorf("127.0.0.1: %d to %q, want 302 to http://localhost:7432/ui/?x=1", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, host := range []string{"attacker.example:7432", "localhost:9999", "localhost"} {
		b.host = host
		if status, _ := b.do("GET", "/ui/", nil); status != http.StatusForbidden {
			t.Errorf("Host %s: %d, want 403", host, status)
		}
	}
	b.host = "attacker.example:7432"
	for _, c := range []struct{ method, path string }{{"POST", "/ui/auth/login/begin"}, {"GET", "/ui/auth/state"}} {
		resp := b.send(c.method, c.path, nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s %s with a foreign Host: %d, want 403 with the security headers", c.method, c.path, resp.StatusCode)
		}
	}
	b.host = testHost
	for name, hdr := range map[string]map[string]string{
		"another origin": {"Origin": "http://attacker.example"},
		"no origin":      {"Origin": ""},
		"127.0.0.1":      {"Origin": "http://127.0.0.1:7432"},
	} {
		resp := b.send("POST", "/ui/auth/login/begin", nil, hdr)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST from %s: %d, want 403", name, resp.StatusCode)
		}
	}
	if status, _ := b.do("POST", "/ui/auth/login/begin", nil); status != http.StatusOK {
		t.Errorf("POST from the UI origin: %d, want 200", status)
	}
}

// Agent-written text must never become markup: the scripts only use
// textContent, and the page has no inline script, style or handler.
func TestAssets_NoMarkupFromData(t *testing.T) {
	banned := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|eval\(|new Function`)
	inline := regexp.MustCompile(`<script>|<script [^>]*>[^<]|<style|\sstyle=|\son[a-z]+=`)
	err := fs.WalkDir(assets, "assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(assets, path)
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".js") && banned.Match(b) {
			t.Errorf("%s: %q", path, banned.Find(b))
		}
		if strings.HasSuffix(path, ".html") && inline.Match(b) {
			t.Errorf("%s: inline %q", path, inline.Find(b))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPages_BodyCapOnAuthRoutes(t *testing.T) {
	_, _, base := newTestUI(t)
	b := newBrowser(t, base)
	status, _ := b.do("POST", "/ui/auth/register/begin", `{"name":"`+strings.Repeat("a", 70<<10)+`"}`)
	if status == http.StatusOK || status == http.StatusCreated {
		t.Errorf("a 70 KiB body: %d, want it refused", status)
	}
}
