package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNew_RequiresToken(t *testing.T) {
	t.Setenv("CONCLAVE_API_KEY", "")
	t.Setenv("CONCLAVE_AGENT", "")
	_, err := New(Options{Server: "http://x"})
	if err == nil {
		t.Fatal("expected error when no token")
	}
}

func TestNew_DefaultsFromEnv(t *testing.T) {
	t.Setenv("CONCLAVE_API_KEY", "env-token")
	t.Setenv("CONCLAVE_SERVER", "http://from-env:1234")
	c, err := New(Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.BaseURL != "http://from-env:1234" {
		t.Errorf("BaseURL=%q", c.BaseURL)
	}
	if c.Token != "env-token" {
		t.Errorf("Token=%q", c.Token)
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	t.Setenv("CONCLAVE_API_KEY", "tok")
	c, err := New(Options{Server: "http://x:1/"})
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "http://x:1" {
		t.Errorf("trim: %q", c.BaseURL)
	}
}

func TestDo_SendsBearerAndDecodesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer abc" {
			t.Errorf("auth header=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type=%q", r.Header.Get("Content-Type"))
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "alpha" {
			t.Errorf("body=%v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "01H", "name": "alpha"})
	}))
	defer srv.Close()

	c, _ := New(Options{Server: srv.URL, Token: "abc"})
	var out struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	status, err := c.Do(context.Background(), Request{
		Method: "POST",
		Path:   "/v1/rooms",
		Body:   map[string]string{"name": "alpha"},
		Out:    &out,
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != 200 {
		t.Errorf("status=%d", status)
	}
	if out.ID != "01H" || out.Name != "alpha" {
		t.Errorf("out=%+v", out)
	}
}

func TestDo_StructuredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "conflict", "message": "name taken"},
		})
	}))
	defer srv.Close()

	c, _ := New(Options{Server: srv.URL, Token: "x"})
	_, err := c.Do(context.Background(), Request{Method: "POST", Path: "/v1/rooms"})
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("expected APIError, got %T %v", err, err)
	}
	if api.Status != 409 || api.Code != "conflict" || api.Message != "name taken" {
		t.Errorf("api=%+v", api)
	}
}

func TestDo_NetworkError(t *testing.T) {
	c, _ := New(Options{Server: "http://127.0.0.1:1", Token: "x", Timeout: 100 * time.Millisecond})
	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/rooms"})
	var net *NetError
	if !errors.As(err, &net) {
		t.Fatalf("expected NetError, got %T %v", err, err)
	}
}

func TestDo_RawBodyFallbackOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	defer srv.Close()
	c, _ := New(Options{Server: srv.URL, Token: "x"})
	_, err := c.Do(context.Background(), Request{Method: "GET", Path: "/v1/rooms"})
	var api *APIError
	if !errors.As(err, &api) {
		t.Fatalf("expected APIError, got %v", err)
	}
	if !strings.Contains(api.Message, "upstream down") {
		t.Errorf("expected raw body in message, got %q", api.Message)
	}
}

func TestIsAuth(t *testing.T) {
	if !IsAuth(&APIError{Status: 401}) {
		t.Error("IsAuth(401) false")
	}
	if IsAuth(&APIError{Status: 403}) {
		t.Error("IsAuth(403) true")
	}
	if IsAuth(errors.New("other")) {
		t.Error("IsAuth(other) true")
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(&APIError{Status: 404}) {
		t.Error("IsNotFound(404) false")
	}
	if IsNotFound(&APIError{Status: 400}) {
		t.Error("IsNotFound(400) true")
	}
}

func TestResolveToken_Order(t *testing.T) {
	old := agentKey
	agentKey = func(label string) (string, error) {
		if label == "missing" {
			return "", errors.New("no stored key")
		}
		return "stored-" + label, nil
	}
	t.Cleanup(func() { agentKey = old })

	cases := []struct {
		name, flagToken, flagAs, envKey, envAgent, want string
		wantErr                                         bool
	}{
		{name: "nothing", want: ""},
		{name: "--token wins", flagToken: "t", flagAs: "a", envKey: "e", envAgent: "b", want: "t"},
		{name: "--as beats env key", flagAs: "a", envKey: "e", want: "stored-a"},
		{name: "env key beats env agent", envKey: "e", envAgent: "b", want: "e"},
		{name: "env agent", envAgent: "b", want: "stored-b"},
		{name: "missing stored key", flagAs: "missing", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONCLAVE_API_KEY", tc.envKey)
			t.Setenv("CONCLAVE_AGENT", tc.envAgent)
			got, err := ResolveToken(tc.flagToken, tc.flagAs)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("ResolveToken = %q, %v; want %q (err %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestNew_UsesAgentKey(t *testing.T) {
	old := agentKey
	agentKey = func(label string) (string, error) { return "stored-" + label, nil }
	t.Cleanup(func() { agentKey = old })
	t.Setenv("CONCLAVE_API_KEY", "")
	t.Setenv("CONCLAVE_AGENT", "")
	c, err := New(Options{Agent: "peer"})
	if err != nil || c.Token != "stored-peer" {
		t.Fatalf("New = %+v, %v", c, err)
	}
}
