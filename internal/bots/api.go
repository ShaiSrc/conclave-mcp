package bots

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/hermes/conclave-mcp/internal/audit"
	"github.com/hermes/conclave-mcp/internal/auth"
)

const maxBody = 64 << 10

// Routes mounts the local bots API on the /v1 router, which
// authenticates every request.
//
//	GET    /bots                   any key: the bots it may see
//	GET    /bots/{handle}          any key: a bot it may see
//	PATCH  /bots/{handle}/profile  admin keys; a bot for its own text fields
//	                               (visibility and dm_policy: admin keys only)
//	PUT    /keys/{id}/handle       admin keys: set or change a key's handle
//	DELETE /keys/{id}/handle       admin keys: remove it
func (s *Store) Routes(r chi.Router) {
	r.Get("/bots", s.handleList)
	r.Get("/bots/{handle}", s.handleGet)
	r.Patch("/bots/{handle}/profile", s.handlePatch)
	r.Put("/keys/{id}/handle", s.handleSetHandle)
	r.Delete("/keys/{id}/handle", s.handleRemoveHandle)
}

func (s *Store) handleList(w http.ResponseWriter, r *http.Request) {
	c, _ := auth.FromContext(r.Context())
	all, err := s.List(r.Context(), Private)
	if err != nil {
		fail(w, r, err)
		return
	}
	out := []Profile{}
	for _, p := range all {
		if Visible(c, p) {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bots": out})
}

// visible returns the bot in the path if the caller may see it, and
// otherwise answers the same 404 as for an unknown handle.
func (s *Store) visible(w http.ResponseWriter, r *http.Request) (Profile, bool) {
	c, _ := auth.FromContext(r.Context())
	h, err := NormalizeHandle(chi.URLParam(r, "handle"))
	if err != nil {
		fail(w, r, ErrNotFound)
		return Profile{}, false
	}
	p, err := s.Get(r.Context(), h)
	if err == nil && !Visible(c, p) {
		err = ErrNotFound
	}
	if err != nil {
		fail(w, r, err)
		return Profile{}, false
	}
	return p, true
}

func (s *Store) handleGet(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.visible(w, r); ok {
		writeJSON(w, http.StatusOK, p)
	}
}

func (s *Store) handlePatch(w http.ResponseWriter, r *http.Request) {
	p, ok := s.visible(w, r)
	if !ok {
		return
	}
	c, _ := auth.FromContext(r.Context())
	admin := auth.IsAdmin(r.Context())
	if !admin && c.KeyID != p.KeyID {
		writeErr(w, r, http.StatusForbidden, "forbidden", "only an admin key or the bot itself edits a profile")
		return
	}
	var patch Patch
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&patch); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON: "+err.Error())
		return
	}
	np, err := s.Update(r.Context(), p.Handle, patch, admin)
	if err != nil {
		fail(w, r, err)
		return
	}
	// The audit names the fields this request changed.
	fields := []string{}
	add := func(name string, changed bool) {
		if changed {
			fields = append(fields, name)
		}
	}
	add("display_name", patch.DisplayName != nil && np.DisplayName != p.DisplayName)
	add("description", patch.Description != nil && np.Description != p.Description)
	add("skills", patch.Skills != nil && !slices.Equal(np.Skills, p.Skills))
	add("visibility", patch.Visibility != nil && np.Visibility != p.Visibility)
	add("dm_policy", patch.DMPolicy != nil && np.DMPolicy != p.DMPolicy)
	detail, _ := json.Marshal(struct {
		Fields     []string `json:"fields"`
		Visibility string   `json:"visibility"`
		DMPolicy   string   `json:"dm_policy"`
	}{fields, np.Visibility, np.DMPolicy})
	_ = audit.Write(r.Context(), s.db, c.KeyID, "bot.profile.update", "bot", np.Handle, "", "ok", string(detail))
	writeJSON(w, http.StatusOK, np)
}

// keyParam returns the {id} of a handle request from an admin key, or
// answers 403 or 400.
func keyParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if !auth.IsAdmin(r.Context()) {
		writeErr(w, r, http.StatusForbidden, "forbidden", "admin key required")
		return 0, false
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "id must be an integer")
		return 0, false
	}
	return id, true
}

func (s *Store) handleSetHandle(w http.ResponseWriter, r *http.Request) {
	id, ok := keyParam(w, r)
	if !ok {
		return
	}
	var in struct {
		Handle string `json:"handle"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&in); err != nil {
		writeErr(w, r, http.StatusBadRequest, "bad_request", "malformed JSON: "+err.Error())
		return
	}
	h, err := NormalizeHandle(in.Handle)
	if err == nil {
		err = s.SetHandle(r.Context(), id, h)
	}
	if err != nil {
		fail(w, r, err)
		return
	}
	s.auditHandle(r, "bot.handle.set", id, h)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "handle": h})
}

func (s *Store) handleRemoveHandle(w http.ResponseWriter, r *http.Request) {
	id, ok := keyParam(w, r)
	if !ok {
		return
	}
	h, err := s.RemoveHandle(r.Context(), id)
	if err != nil {
		fail(w, r, err)
		return
	}
	s.auditHandle(r, "bot.handle.remove", id, h)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Store) auditHandle(r *http.Request, action string, keyID int64, handle string) {
	c, _ := auth.FromContext(r.Context())
	detail, _ := json.Marshal(map[string]string{"handle": handle})
	_ = audit.Write(r.Context(), s.db, c.KeyID, action, "key", strconv.FormatInt(keyID, 10), "", "ok", string(detail))
}

func fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := http.StatusInternalServerError, "internal_error", "internal error"
	switch {
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, ErrInvalid):
		status, code = http.StatusBadRequest, "bad_request"
	case errors.Is(err, ErrTaken):
		status, code = http.StatusConflict, "handle_taken"
	case errors.Is(err, ErrAdminKey):
		status, code = http.StatusBadRequest, "admin_key"
	case errors.Is(err, ErrForbidden):
		status, code = http.StatusForbidden, "forbidden"
	}
	if status != http.StatusInternalServerError {
		msg = err.Error()
	}
	writeErr(w, r, status, code, msg)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeErr writes the SPEC-0003 §5 error envelope.
func writeErr(w http.ResponseWriter, _ *http.Request, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": msg, "request_id": w.Header().Get("X-Request-Id"),
	}})
}
