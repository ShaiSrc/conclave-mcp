package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeLookup struct {
	keyID  int64
	scopes []string
	phc    string
	err    error
}

func (f fakeLookup) LookupByPrefix(ctx context.Context, prefix string) (int64, []string, string, error) {
	return f.keyID, f.scopes, f.phc, f.err
}

func TestGenerateToken_Format(t *testing.T) {
	tok, phc, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if !strings.HasPrefix(tok, "tk_") {
		t.Errorf("token: missing tk_ prefix: %q", tok)
	}
	if len(tok) != 46 {
		t.Errorf("token length: got %d, want 46", len(tok))
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$") {
		t.Errorf("phc: unexpected format: %q", phc)
	}
}

func TestGenerateToken_Unique(t *testing.T) {
	a, _, _ := GenerateToken()
	b, _, _ := GenerateToken()
	if a == b {
		t.Errorf("two tokens were identical: %q", a)
	}
}

func TestVerifyPHC_Roundtrip(t *testing.T) {
	tok, phc, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	ok, err := VerifyPHC(tok, phc)
	if err != nil {
		t.Fatalf("VerifyPHC: %v", err)
	}
	if !ok {
		t.Error("VerifyPHC returned false for matching token")
	}
}

func TestVerifyPHC_WrongToken(t *testing.T) {
	_, phc, _ := GenerateToken()
	other, _, _ := GenerateToken()
	ok, err := VerifyPHC(other, phc)
	if err != nil {
		t.Fatalf("VerifyPHC: %v", err)
	}
	if ok {
		t.Error("VerifyPHC returned true for non-matching token")
	}
}

func TestVerifyPHC_BadFormat(t *testing.T) {
	_, err := VerifyPHC("tk_x", "not a phc string")
	if err == nil {
		t.Error("expected error for malformed PHC, got nil")
	}
}

func TestVerify_BearerMissing(t *testing.T) {
	v := NewVerifier(fakeLookup{}, 1)
	_, _, err := v.Verify(context.Background(), "")
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("missing header: got %v, want ErrInvalid", err)
	}
}

func TestVerify_BearerWrongScheme(t *testing.T) {
	v := NewVerifier(fakeLookup{}, 1)
	_, _, err := v.Verify(context.Background(), "Basic dXNlcjpwYXNz")
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("basic auth: got %v, want ErrInvalid", err)
	}
}

func TestVerify_BearerNoPrefix(t *testing.T) {
	v := NewVerifier(fakeLookup{}, 1)
	_, _, err := v.Verify(context.Background(), "Bearer 12345678")
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("no tk_ prefix: got %v, want ErrInvalid", err)
	}
}

func TestVerify_BearerShortToken(t *testing.T) {
	v := NewVerifier(fakeLookup{}, 1)
	_, _, err := v.Verify(context.Background(), "Bearer tk_short")
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("short token: got %v, want ErrInvalid", err)
	}
}

func TestVerify_LookupReturnsNotFound(t *testing.T) {
	v := NewVerifier(fakeLookup{err: ErrNotFound}, 1)
	tok, _, _ := GenerateToken()
	_, _, err := v.Verify(context.Background(), "Bearer "+tok)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("not-found: got %v, want ErrInvalid", err)
	}
}

func TestVerify_LookupReturnsWrongPHC(t *testing.T) {
	// Lookup returns a PHC that does NOT match the bearer.
	_, otherPHC, _ := GenerateToken()
	v := NewVerifier(fakeLookup{keyID: 1, phc: otherPHC}, 1)
	tok, _, _ := GenerateToken()
	_, _, err := v.Verify(context.Background(), "Bearer "+tok)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("wrong PHC: got %v, want ErrInvalid", err)
	}
}

func TestVerify_HappyPath(t *testing.T) {
	tok, phc, _ := GenerateToken()
	v := NewVerifier(fakeLookup{keyID: 42, scopes: []string{"rooms:read"}, phc: phc}, 1)
	keyID, scopes, err := v.Verify(context.Background(), "Bearer "+tok)
	if err != nil {
		t.Fatalf("happy: %v", err)
	}
	if keyID != 42 {
		t.Errorf("keyID: got %d, want 42", keyID)
	}
	if len(scopes) != 1 || scopes[0] != "rooms:read" {
		t.Errorf("scopes: got %v", scopes)
	}
}

func TestVerify_SemaphoreBlocks(t *testing.T) {
	// Hold the semaphore by filling it; Verify should block until
	// we release.
	tok, phc, _ := GenerateToken()
	v := NewVerifier(fakeLookup{keyID: 1, phc: phc}, 1)
	v.sem <- struct{}{} // slot taken

	done := make(chan error, 1)
	go func() {
		_, _, err := v.Verify(context.Background(), "Bearer "+tok)
		done <- err
	}()

	// Verify should be blocked.
	select {
	case err := <-done:
		t.Errorf("Verify did not block: err=%v", err)
	default:
	}

	// Release the slot; Verify should now complete.
	<-v.sem
	if err := <-done; err != nil {
		t.Errorf("Verify after release: %v", err)
	}
}

func TestVerify_ContextCancelDuringWait(t *testing.T) {
	tok, phc, _ := GenerateToken()
	v := NewVerifier(fakeLookup{keyID: 1, phc: phc}, 1)
	v.sem <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := v.Verify(ctx, "Bearer "+tok)
		done <- err
	}()

	cancel()
	if err := <-done; err == nil {
		t.Error("expected context.Canceled, got nil")
	}
	<-v.sem // cleanup
}

// mutableLookup lets a test revoke or re-hash a key between calls.
type mutableLookup struct{ f fakeLookup }

func (m *mutableLookup) LookupByPrefix(ctx context.Context, prefix string) (int64, []string, string, error) {
	return m.f.LookupByPrefix(ctx, prefix)
}

func TestVerify_CacheStillHonoursRevocationAndRehash(t *testing.T) {
	tok, phc, _ := GenerateToken()
	lk := &mutableLookup{fakeLookup{keyID: 7, phc: phc}}
	v := NewVerifier(lk, 1)
	for i := 0; i < 2; i++ { // second call is served from the cache
		if id, _, err := v.Verify(context.Background(), "Bearer "+tok); err != nil || id != 7 {
			t.Fatalf("call %d: id=%d err=%v", i, id, err)
		}
	}
	v.sem <- struct{}{} // a cache hit must not need a verify slot
	if _, _, err := v.Verify(context.Background(), "Bearer "+tok); err != nil {
		t.Fatalf("cached verify blocked or failed: %v", err)
	}
	<-v.sem

	_, otherPHC, _ := GenerateToken()
	lk.f.phc = otherPHC
	if _, _, err := v.Verify(context.Background(), "Bearer "+tok); !errors.Is(err, ErrInvalid) {
		t.Errorf("after re-hash: got %v, want ErrInvalid", err)
	}
	lk.f = fakeLookup{err: ErrNotFound} // revoked
	if _, _, err := v.Verify(context.Background(), "Bearer "+tok); !errors.Is(err, ErrInvalid) {
		t.Errorf("after revoke: got %v, want ErrInvalid", err)
	}
}
