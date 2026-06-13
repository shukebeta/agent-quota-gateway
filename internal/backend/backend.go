// Package backend holds the gateway's registry of named upstream
// credentials and the request-scoped resolution of an inbound selector
// to one of them.
//
// The gateway owns every upstream credential. A client never sends a
// real token: it sends a *selector* (via ANTHROPIC_AUTH_TOKEN, which
// Claude Code puts on the Authorization header), and the gateway swaps
// in the selected backend's credential before forwarding. Backends are
// declared purely through the process environment — there is no
// credential file, so the gateway keeps its "no on-disk state" posture.
package backend

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// EnvPrefix marks an environment variable as a backend declaration:
// AQG_BACKEND_<NICK>=<credential>. The part after the prefix is
// normalized into the selector clients use (see normalizeNick).
const EnvPrefix = "AQG_BACKEND_"

// Backend is one resolved upstream identity. Credential is the real
// secret the proxy stamps outbound; Nick is the stable key the selector
// resolves to and the quota store files snapshots under.
type Backend struct {
	Nick       string
	Credential string
}

// Registry maps selector nicks to backends. It is immutable after Load
// and safe for concurrent reads.
type Registry struct {
	byNick map[string]Backend
}

// Load builds a Registry from AQG_BACKEND_* environment variables.
//
// It fails closed: an empty credential, two declarations that normalize
// to the same nick, or no backends at all are all startup errors rather
// than a gateway that silently can't route. The credential value itself
// is never included in an error message.
func Load() (*Registry, error) {
	return loadFrom(os.Environ())
}

// loadFrom is Load's testable core: it takes "KEY=VALUE" entries in the
// same shape as os.Environ().
func loadFrom(environ []string) (*Registry, error) {
	byNick := make(map[string]Backend)
	// originKey records which env var produced each nick so a collision
	// error can name both sides.
	originKey := make(map[string]string)

	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		if !strings.HasPrefix(key, EnvPrefix) {
			continue
		}
		rawNick := strings.TrimPrefix(key, EnvPrefix)
		nick := normalizeNick(rawNick)
		if nick == "" {
			return nil, fmt.Errorf("backend: %s has an empty nick", key)
		}
		if val == "" {
			return nil, fmt.Errorf("backend: %s has an empty credential", key)
		}
		if prev, dup := originKey[nick]; dup {
			return nil, fmt.Errorf("backend: %s and %s both map to nick %q", prev, key, nick)
		}
		originKey[nick] = key
		byNick[nick] = Backend{Nick: nick, Credential: val}
	}

	if len(byNick) == 0 {
		return nil, fmt.Errorf("backend: no backends configured; set at least one %s<NICK>", EnvPrefix)
	}
	return &Registry{byNick: byNick}, nil
}

// Resolve returns the backend a selector names. The selector is matched
// case-insensitively against the normalized nick. ok is false when no
// backend matches — the caller must fail closed rather than fall back.
func (r *Registry) Resolve(selector string) (Backend, bool) {
	b, ok := r.byNick[normalizeSelector(selector)]
	return b, ok
}

// Nicks returns the configured nicks in sorted order. Intended for
// startup logging and diagnostics — it exposes names, never credentials.
func (r *Registry) Nicks() []string {
	out := make([]string, 0, len(r.byNick))
	for nick := range r.byNick {
		out = append(out, nick)
	}
	sort.Strings(out)
	return out
}

// normalizeNick canonicalizes the env-key suffix into a selector nick:
// lowercase, with underscores folded to hyphens so AQG_BACKEND_CLAUDE_A
// is addressed as "claude-a". Surrounding hyphens are trimmed.
func normalizeNick(raw string) string {
	n := strings.ToLower(strings.TrimSpace(raw))
	n = strings.ReplaceAll(n, "_", "-")
	return strings.Trim(n, "-")
}

// normalizeSelector canonicalizes an inbound selector the same way a
// nick is canonicalized, so the value a client puts in
// ANTHROPIC_AUTH_TOKEN matches the configured nick regardless of case.
func normalizeSelector(sel string) string {
	return normalizeNick(sel)
}

// ctxKey is unexported so no other package can collide with our context
// value.
type ctxKey struct{}

// WithBackend returns a copy of ctx carrying b, for the proxy director
// and quota observer to read after the resolver middleware runs.
func WithBackend(ctx context.Context, b Backend) context.Context {
	return context.WithValue(ctx, ctxKey{}, b)
}

// FromContext returns the backend stored by WithBackend. ok is false
// when no backend was resolved for the request.
func FromContext(ctx context.Context) (Backend, bool) {
	b, ok := ctx.Value(ctxKey{}).(Backend)
	return b, ok
}
