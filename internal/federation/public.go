package federation

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/bots"
)

//go:embed public/*
var publicFS embed.FS

// maxPublicBots is the most bots the directory page lists.
const maxPublicBots = 1000

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
		r.Get("/public.css", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			_, _ = w.Write(css)
		})
	})
}

type skillView struct{ ID, Name, Description string }

type botView struct {
	Handle, Name, Address, Description string
	Skills                             []skillView
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

func (s *Service) handlePublicBot(w http.ResponseWriter, r *http.Request) {
	p, ok, err := s.publicBot(r)
	switch {
	case err != nil:
		s.internalError(w, "public bot", err)
	case !ok:
		s.notFound(w)
	default:
		v := s.view(p)
		s.render(w, http.StatusOK, "bot", pageData{Title: v.Name + " · " + v.Address, Foot: v.Address, Bot: &v})
	}
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
