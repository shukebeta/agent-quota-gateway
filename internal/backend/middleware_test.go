package backend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := loadFrom([]string{"AQG_BACKEND_CLAUDE_A=cred-a"})
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	return reg
}

func TestMiddleware_resolvesAndInjects(t *testing.T) {
	var seen Backend
	var called bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		seen, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(testRegistry(t), next)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer claude-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatal("next handler not called for a valid selector")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if seen.Nick != "claude-a" || seen.Credential != "cred-a" {
		t.Errorf("injected backend = %+v, want {claude-a cred-a}", seen)
	}
}

func TestMiddleware_failsClosed(t *testing.T) {
	cases := []struct {
		name string
		auth string // Authorization header; "" means unset
	}{
		{"unknown selector", "Bearer claude-z"},
		{"missing header", ""},
		{"empty bearer", "Bearer "},
		{"non-bearer scheme", "Basic claude-a"},
		{"raw token no scheme", "claude-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next must not be called on a fail-closed request")
			})
			h := Middleware(testRegistry(t), next)

			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			// The rejected selector value must never appear in the body.
			if strings.Contains(rec.Body.String(), "claude") {
				t.Errorf("response body leaked selector/config: %q", rec.Body.String())
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":   "abc",
		"bearer abc":   "abc", // scheme is case-insensitive
		"BEARER  abc ": "abc", // surrounding space trimmed
		"Basic abc":    "",
		"abc":          "",
		"":             "",
		"Bearer":       "",
	}
	for in, want := range cases {
		if got := bearerToken(in); got != want {
			t.Errorf("bearerToken(%q) = %q, want %q", in, got, want)
		}
	}
}
