package federation

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/bots"
)

//go:embed public/*
var publicFS embed.FS

// maxPublicBots is the most bots the directory page lists.
const maxPublicBots = 1000

// maxPublicPosts is the most posts a bot page and its feed show.
const maxPublicPosts = 20

var (
	pages = map[string]*template.Template{}
	css   []byte
)

func init() {
	for _, name := range []string{"directory", "bot", "notfound"} {
		pages[name] = template.Must(template.ParseFS(publicFS, "public/layout.html", "public/"+name+".html"))
	}
	css, _ = publicFS.ReadFile("public/public.css")
}

// publicHeaders sets the headers every response of the federation
// listener carries: no script, no style but /public.css, no image, no
// framing, no referrer.
func publicHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// noCache makes caches revalidate a public page, so a bot no longer
// public leaves them at once.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// publicRoutes mounts the pages of public bots. They need no
// signature and show nothing but public profiles.
func (s *Service) publicRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(noCache)
		r.Get("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/directory", http.StatusFound) })
		r.Get("/directory", s.handlePublicDirectory)
		r.Get("/@{name}", s.handlePublicBot)
		r.Get("/@{name}/agent-card.json", s.handlePublicCard)
		r.Get("/@{name}/feed.xml", s.handlePublicFeed)
		r.Get("/public.css", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			_, _ = w.Write(css)
		})
	})
}

type skillView struct{ ID, Name, Description string }

type postView struct {
	ID   int64
	TS   time.Time
	Text string
}

type botView struct {
	Handle, Name, Address, Description string
	Skills                             []skillView
	Posts                              []postView // on the bot page only
}

type pageData struct {
	Title, Domain, Foot string
	Bots                []botView
	Bot                 *botView
}

func (s *Service) view(p bots.Profile) botView {
	v := botView{Handle: p.Handle, Name: p.DisplayName, Address: p.Handle + "@" + s.id.Domain, Description: p.Description}
	if v.Name == "" {
		v.Name = p.Handle
	}
	for _, sk := range p.Skills {
		v.Skills = append(v.Skills, skillView{sk.ID, sk.Name, sk.Description})
	}
	return v
}

func (s *Service) render(w http.ResponseWriter, status int, page string, d pageData) {
	d.Domain = s.id.Domain
	var buf bytes.Buffer
	if err := pages[page].ExecuteTemplate(&buf, "layout", d); err != nil {
		s.logger.Error("public page", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// notFound is the page for unknown and non-public bots alike.
func (s *Service) notFound(w http.ResponseWriter) {
	s.render(w, http.StatusNotFound, "notfound", pageData{Title: "Not found", Foot: s.id.Domain})
}

func (s *Service) internalError(w http.ResponseWriter, what string, err error) {
	s.logger.Error(what, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Service) handlePublicDirectory(w http.ResponseWriter, r *http.Request) {
	all, err := s.botStore.List(r.Context(), bots.Public)
	if err != nil {
		s.internalError(w, "public directory", err)
		return
	}
	d := pageData{Title: "Bots on " + s.id.Domain, Foot: s.id.Domain}
	for _, p := range all[:min(len(all), maxPublicBots)] {
		d.Bots = append(d.Bots, s.view(p))
	}
	s.render(w, http.StatusOK, "directory", d)
}

// publicBot returns the public bot named in the URL; ok is false for
// an unknown, malformed or non-public name.
func (s *Service) publicBot(r *http.Request) (p bots.Profile, ok bool, err error) {
	h, err := bots.NormalizeHandle(chi.URLParam(r, "name"))
	if err != nil {
		return p, false, nil
	}
	p, err = s.botStore.Get(r.Context(), h)
	switch {
	case errors.Is(err, bots.ErrNotFound):
		return p, false, nil
	case err != nil:
		return p, false, err
	}
	return p, p.Visibility == bots.Public, nil
}

// publicPosts returns the newest public posts of p, a bot of ours:
// its own undeleted messages in its active feed whose metadata says
// public, newest first.
func (s *Service) publicPosts(ctx context.Context, p bots.Profile) ([]postView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.ts, m.content FROM messages m
		JOIN rooms r ON r.id = m.room_id AND r.feed = ? AND r.home_domain IS NULL AND r.archived_at IS NULL
		WHERE m.author_key_id = ? AND m.deleted_at IS NULL
		AND CASE WHEN json_valid(m.metadata) THEN json_extract(m.metadata, '$.visibility') END = 'public'
		ORDER BY m.ts DESC, m.id DESC LIMIT ?`, p.Handle+"@"+s.id.Domain, p.KeyID, maxPublicPosts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []postView
	for rows.Next() {
		var (
			v  postView
			ts string
		)
		if err := rows.Scan(&v.ID, &ts, &v.Text); err != nil {
			return nil, err
		}
		if v.TS, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Service) handlePublicBot(w http.ResponseWriter, r *http.Request) {
	p, ok, err := s.publicBot(r)
	v := s.view(p)
	if err == nil && ok {
		v.Posts, err = s.publicPosts(r.Context(), p)
	}
	switch {
	case err != nil:
		s.internalError(w, "public bot", err)
	case !ok:
		s.notFound(w)
	default:
		s.render(w, http.StatusOK, "bot", pageData{Title: v.Name + " · " + v.Address, Foot: v.Address, Bot: &v})
	}
}

type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Author  atomAuthor  `xml:"author"`
	Links   []atomLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

type atomAuthor struct {
	Name string `xml:"name"`
	URI  string `xml:"uri"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
	Href string `xml:"href,attr"`
}

type atomEntry struct {
	ID      string   `xml:"id"`
	Title   string   `xml:"title"`
	Updated string   `xml:"updated"`
	Content atomText `xml:"content"`
}

type atomText struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

// handlePublicFeed serves the posts of the bot page as an Atom feed.
// encoding/xml escapes the text and replaces characters XML forbids.
func (s *Service) handlePublicFeed(w http.ResponseWriter, r *http.Request) {
	p, ok, err := s.publicBot(r)
	var posts []postView
	if err == nil && ok {
		posts, err = s.publicPosts(r.Context(), p)
	}
	switch {
	case err != nil:
		s.internalError(w, "public feed", err)
		return
	case !ok:
		s.notFound(w)
		return
	}
	v, page := s.view(p), "https://"+s.id.Domain+"/@"+p.Handle
	f := atomFeed{ID: "tag:" + s.id.Domain + ",2026:feed/" + p.Handle, Title: v.Name,
		Updated: p.UpdatedAt.UTC().Format(time.RFC3339), Author: atomAuthor{v.Name, page},
		Links: []atomLink{{"self", "application/atom+xml", page + "/feed.xml"}, {"alternate", "text/html", page}}}
	for i, post := range posts {
		updated := post.TS.UTC().Format(time.RFC3339)
		if i == 0 {
			f.Updated = updated
		}
		title, _, _ := strings.Cut(post.Text, "\n")
		title = strings.TrimSpace(title)
		if rs := []rune(title); len(rs) > 80 {
			title = string(rs[:80])
		}
		f.Entries = append(f.Entries, atomEntry{ID: fmt.Sprintf("tag:%s,2026:post/%d", s.id.Domain, post.ID), Title: title,
			Updated: updated, Content: atomText{"text", post.Text}})
	}
	body, err := xml.Marshal(f)
	if err != nil {
		s.internalError(w, "public feed", err)
		return
	}
	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	_, _ = w.Write(append([]byte(xml.Header), body...))
}

// handlePublicCard serves the bot's A2A agent card, the subset
// described in the bot profiles design.
func (s *Service) handlePublicCard(w http.ResponseWriter, r *http.Request) {
	p, ok, err := s.publicBot(r)
	switch {
	case err != nil:
		s.internalError(w, "public agent card", err)
		return
	case !ok:
		s.notFound(w)
		return
	}
	v := s.view(p)
	skills := append([]bots.Skill{}, p.Skills...)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":               v.Name,
		"description":        p.Description,
		"version":            p.UpdatedAt.UTC().Format("2006-01-02"),
		"skills":             skills,
		"defaultInputModes":  []string{"text/plain"},
		"defaultOutputModes": []string{"text/plain"},
		"capabilities":       map[string]any{},
		"supportedInterfaces": []map[string]string{{
			"url": "https://" + s.id.Domain + "/@" + p.Handle, "protocolBinding": "conclave-federation/1", "protocolVersion": "1"}},
		"x-conclave": map[string]string{"address": v.Address},
	})
}
