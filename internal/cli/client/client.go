// Package client provides a tiny HTTP client used by the conclave-mcp
// CLI subcommands to talk to a running server.
//
// Scope (per SPEC-0003 + SPEC-0004):
//   - Bearer token auth (CONCLAVE_API_KEY env or --token flag)
//   - Server URL from --server flag or CONCLAVE_SERVER env
//   - JSON request/response with X-Request-Id propagation
//   - 2xx -> return parsed body + status
//   - 4xx/5xx -> return typed error carrying server's error code + message
//   - Network/timeout errors return *NetError so the caller can map to exit 3
//
// This package deliberately has no business logic. It is the CLI's
// transport, not its policy.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Default timeout for any single HTTP request.
const DefaultTimeout = 15 * time.Second

// Client wraps net/http.Client with conclave-specific defaults.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
	Logger  ErrorLogger
	Out     io.Writer // for -v verbose logs (currently unused but reserved)
}

// ErrorLogger is satisfied by *slog.Logger but kept as an interface so
// tests can pass a no-op or a recorder.
type ErrorLogger interface {
	Error(msg string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Error(string, ...any) {}

// Options configures a Client. Zero values use sensible defaults.
type Options struct {
	Server  string // full URL like http://127.0.0.1:7432
	Token   string // bearer token (without "Bearer " prefix)
	Timeout time.Duration
	Logger  ErrorLogger
}

// New constructs a Client. Empty Options fields fall back to env vars
// (CONCLAVE_SERVER or CONCLAVE_BASE_URL for the URL, CONCLAVE_API_KEY
// for the token) and DefaultTimeout. CONCLAVE_BASE_URL is the
// spec-mandated name (SPEC-0004 §3); CONCLAVE_SERVER is the
// historical alias kept for backward compatibility.
func New(opts Options) (*Client, error) {
	if opts.Server == "" {
		opts.Server = os.Getenv("CONCLAVE_SERVER")
	}
	if opts.Server == "" {
		// SPEC-0004 §3: the env var is called CONCLAVE_BASE_URL.
		// We accept it as the canonical name; CONCLAVE_SERVER
		// above is the legacy alias.
		opts.Server = os.Getenv("CONCLAVE_BASE_URL")
	}
	if opts.Server == "" {
		opts.Server = "http://127.0.0.1:7432"
	}
	opts.Server = strings.TrimRight(opts.Server, "/")
	if opts.Token == "" {
		opts.Token = os.Getenv("CONCLAVE_API_KEY")
	}
	if opts.Token == "" {
		return nil, errors.New("api token required: pass --token or set CONCLAVE_API_KEY")
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Logger == nil {
		opts.Logger = nopLogger{}
	}
	return &Client{
		HTTP:    &http.Client{Timeout: opts.Timeout},
		BaseURL: opts.Server,
		Token:   opts.Token,
		Logger:  opts.Logger,
	}, nil
}

// Request is a small wrapper: method, path, optional body, optional
// out-pointer for JSON decoding. The body, if non-nil, is JSON-encoded.
type Request struct {
	Method string
	Path   string
	Body   any
	Out    any
}

// Do sends the request and decodes the response.
//
// Exit-code mapping (for cmd/* callers):
//
//	2xx                  -> nil, status populated, Out populated if non-nil
//	4xx (non-401)        -> *APIError (caller decides: 1 or 2)
//	401                  -> *APIError with Code="unauthenticated"
//	5xx, network, parse  -> *APIError or *NetError
func (c *Client) Do(ctx context.Context, req Request) (status int, err error) {
	var body io.Reader
	if req.Body != nil {
		buf, mErr := json.Marshal(req.Body)
		if mErr != nil {
			return 0, fmt.Errorf("marshal request: %w", mErr)
		}
		body = bytes.NewReader(buf)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, c.BaseURL+req.Path, body)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	httpReq.Header.Set("Accept", "application/json")
	if req.Body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return 0, &NetError{Err: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, &NetError{Err: fmt.Errorf("read body: %w", err)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to decode the structured error envelope, fall back to raw body.
		var env struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if jErr := json.Unmarshal(respBody, &env); jErr == nil && env.Error.Code != "" {
			return resp.StatusCode, &APIError{
				Status:  resp.StatusCode,
				Code:    env.Error.Code,
				Message: env.Error.Message,
			}
		}
		return resp.StatusCode, &APIError{
			Status:  resp.StatusCode,
			Code:    "unknown",
			Message: strings.TrimSpace(string(respBody)),
		}
	}

	if req.Out != nil && len(respBody) > 0 {
		if jErr := json.Unmarshal(respBody, req.Out); jErr != nil {
			return resp.StatusCode, &NetError{Err: fmt.Errorf("decode response: %w", jErr)}
		}
	}
	return resp.StatusCode, nil
}

// APIError represents a 4xx/5xx response with a structured error envelope.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("server returned %d (%s): %s", e.Status, e.Code, e.Message)
}

// NetError wraps network, timeout, or decode failures.
type NetError struct {
	Err error
}

func (e *NetError) Error() string { return "network error: " + e.Err.Error() }
func (e *NetError) Unwrap() error { return e.Err }

// IsAuth reports whether err is a 401. Callers map this to exit code 1
// (server rejected our key) per SPEC-0004 §4.
func IsAuth(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusUnauthorized
}

// IsNotFound reports whether err is a 404. Used by "get" commands to
// decide whether to print "not found" (exit 1) vs network error (exit 3).
func IsNotFound(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusNotFound
}
