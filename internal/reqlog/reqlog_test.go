package reqlog

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// setDebug overrides the package-level enable gate for the duration of a test,
// restoring it afterwards. The gate is normally read once from the environment
// at init, so tests flip it directly rather than via env.
func setDebug(t *testing.T, v bool) {
	t.Helper()
	old := debugEnabled
	debugEnabled = v
	t.Cleanup(func() { debugEnabled = old })
}

// captureStderr redirects os.Stderr through a pipe while fn runs and returns
// what was written. The package writes dumps directly to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return string(out)
}

// recordingTransport is a stub RoundTripper that records the request it saw.
type recordingTransport struct {
	seen   *http.Request
	called bool
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.called = true
	rt.seen = r
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func TestWrapTransport_disabledReturnsInner(t *testing.T) {
	setDebug(t, false)
	inner := &recordingTransport{}
	if got := WrapTransport(inner); got != http.RoundTripper(inner) {
		t.Errorf("disabled WrapTransport should return inner unchanged, got %T", got)
	}
}

func TestWrapTransport_enabledRedactsAndDelegates(t *testing.T) {
	setDebug(t, true)
	inner := &recordingTransport{}
	rt := WrapTransport(inner)
	if rt == http.RoundTripper(inner) {
		t.Fatal("enabled WrapTransport should wrap, not return inner")
	}

	req, _ := http.NewRequest(http.MethodGet, "https://upstream.example/v1", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("X-Api-Key", "sk-secret")
	req.Header.Set("X-Custom", "passthrough")

	out := captureStderr(t, func() {
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
	})

	if !inner.called {
		t.Error("inner RoundTripper was not called")
	}
	if strings.Contains(out, "secret-token") || strings.Contains(out, "sk-secret") {
		t.Errorf("credential leaked into dump:\n%s", out)
	}
	for _, want := range []string{"Authorization: [redacted]", "X-Api-Key: [redacted]", "X-Custom: passthrough"} {
		if !strings.Contains(out, want) {
			t.Errorf("dump missing %q:\n%s", want, out)
		}
	}
}

func TestMiddleware_disabledReturnsNext(t *testing.T) {
	setDebug(t, false)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	got := Middleware(next)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(next).Pointer() {
		t.Error("disabled Middleware should return next unchanged")
	}
}

func TestMiddleware_enabledRedactsAndRestoresBody(t *testing.T) {
	setDebug(t, true)

	var gotBody string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte("hello-body")))
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("X-Custom", "passthrough")

	out := captureStderr(t, func() {
		Middleware(next).ServeHTTP(httptest.NewRecorder(), req)
	})

	if gotBody != "hello-body" {
		t.Errorf("downstream body = %q, want it restored to %q", gotBody, "hello-body")
	}
	if strings.Contains(out, "secret-token") {
		t.Errorf("credential leaked into dump:\n%s", out)
	}
	if !strings.Contains(out, "Authorization: [redacted]") {
		t.Errorf("dump missing redacted Authorization:\n%s", out)
	}
	if !strings.Contains(out, "X-Custom: passthrough") {
		t.Errorf("dump missing passthrough header:\n%s", out)
	}
	if !strings.Contains(out, "hello-body") {
		t.Errorf("dump missing body preview:\n%s", out)
	}
}

func TestMiddleware_enabledTruncatesLargeBody(t *testing.T) {
	setDebug(t, true)

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	large := strings.Repeat("x", 600)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(large)))

	out := captureStderr(t, func() {
		Middleware(next).ServeHTTP(httptest.NewRecorder(), req)
	})

	if !strings.Contains(out, "600 bytes total)") {
		t.Errorf("large body should report total size with truncation suffix:\n%s", out)
	}
	// Only the first 500 bytes are previewed, so the full 600-char run must not appear.
	if strings.Contains(out, large) {
		t.Errorf("full body should be truncated in the dump:\n%s", out)
	}
}
