// Package backend holds the gateway's registry of named upstream pools
// and the request-scoped resolution of an inbound selector to a backend
// within one of them.
//
// The gateway owns every upstream credential. A client never sends a
// real token: it sends a *pool name* (via ANTHROPIC_AUTH_TOKEN, which
// Claude Code puts on the Authorization header), and the gateway picks a
// backend from that pool and swaps in its credential before forwarding.
//
// Everything is a pool. There is no non-pool mode and no implicit
// default pool: every backend is declared inside a named pool through
// the process environment. A pool groups *interchangeable* backends —
// same protocol, same quota semantics — so the auto-rotation that fronts
// every pool can fail over between its members without changing the
// observable model or quota behaviour. Backends are declared purely
// through environment variables; there is no credential file, so the
// gateway keeps its "no on-disk state" posture.
package backend

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EnvPrefix marks an environment variable as belonging to the pool
// configuration namespace. Recognised shapes under it:
//
//	AQG_POOL_<POOL>_BASE_URL=<upstream>            // the pool's default upstream
//	AQG_POOL_<POOL>_BACKEND_<NICK>=<cred>[|<url>]  // one member of the pool
//	AQG_POOL_<POOL>_PRIORITY=<nick>,<nick>,...     // ordered preference (optional)
//	AQG_POOL_<POOL>_BALANCE=lead                   // opt-in balanced routing (optional)
//	AQG_POOL_<POOL>_BALANCE_GAP=<fraction>         // min lead gap to trigger a switch
//	AQG_POOL_<POOL>_BALANCE_DWELL=<duration>       // min time between balance switches
//
// <POOL> and <NICK> are normalized (see normalizeName): lowercased, with
// underscores folded to hyphens, so AQG_POOL_Z_AI_BACKEND_KEY_A is
// addressed as pool "z-ai", member "key-a".
//
// PRIORITY is optional and opt-in: when present, the pool prefers its
// listed members in order (highest first) for the auto controller's
// initial pick and failover target; when absent, the pool keeps the
// default random-start, round-robin behaviour. It carries no credential.
// PRIORITY and BALANCE are mutually exclusive — declaring both on the
// same pool is a startup error.
const EnvPrefix = "AQG_POOL_"

// baseURLSuffix, backendInfix, prioritySuffix, and the balance suffixes
// are the structural markers inside an EnvPrefix key. backendInfix is
// checked first so a member declaration always wins over the suffix shapes.
const (
	baseURLSuffix      = "_BASE_URL"
	backendInfix       = "_BACKEND_"
	prioritySuffix     = "_PRIORITY"
	balanceSuffix      = "_BALANCE"
	balanceGapSuffix   = "_BALANCE_GAP"
	balanceDwellSuffix = "_BALANCE_DWELL"
)

// priorityListSep separates nicks in an AQG_POOL_<POOL>_PRIORITY value.
const priorityListSep = ","

// defaultBalanceGap is the minimum lead difference (active minus candidate)
// that triggers a balance switch when AQG_POOL_<POOL>_BALANCE_GAP is absent.
// 0.15 means the active member must be consuming quota at least 15% faster
// than the candidate (relative to elapsed window fraction) before switching.
const defaultBalanceGap = 0.15

// defaultBalanceDwell is the minimum time between balance switches when
// AQG_POOL_<POOL>_BALANCE_DWELL is absent. 5 minutes bounds switches to
// at most 12 per hour per pool, limiting prompt-cache disruption.
const defaultBalanceDwell = 5 * time.Minute

// credURLSep splits a member's value into its credential and an optional
// per-member base-URL override: <credential>|<url>. A credential that
// itself contains this byte is rejected at load because the tail must
// then parse as a URL and won't.
const credURLSep = '|'

// Backend is one resolved upstream identity within a pool. Credential is
// the real secret the proxy stamps outbound; Nick is the stable per-pool
// handle the quota store and logs use; BaseURL is the upstream this
// backend forwards to (the pool default, or a per-member override). Pool
// is the client-facing selector this backend belongs to.
//
// All fields are strings so Backend stays comparable and safe to carry
// by value on a request context.
type Backend struct {
	Pool       string
	Nick       string
	Credential string
	BaseURL    string
}

// QuotaKey is the stable key the quota store files this backend's
// snapshots under. Nicks are unique only within a pool, so the key is
// qualified by pool to stay globally unique.
func (b Backend) QuotaKey() string {
	return b.Pool + "/" + b.Nick
}

// Registry maps pool names to their members. It is immutable after Load
// and safe for concurrent reads.
type Registry struct {
	pools map[string]*pool
}

// pool is one named, immutable set of interchangeable backends.
type pool struct {
	name   string
	byNick map[string]Backend
	nicks  []string // sorted, stable order for the auto controller

	// priority is the operator-declared preference order (highest first),
	// a subset of nicks. nil when the pool declared no AQG_POOL_<POOL>_PRIORITY
	// — that pool keeps the default random-start, round-robin behaviour.
	priority []string

	// balance is the routing mode when the pool opted into balanced routing
	// via AQG_POOL_<POOL>_BALANCE. Currently only "lead" is supported; ""
	// means balanced mode is off. Mutually exclusive with priority.
	balance string
	// balanceGap is the minimum lead difference (active minus candidate)
	// that triggers a balance switch. Set to defaultBalanceGap when
	// AQG_POOL_<POOL>_BALANCE_GAP is absent.
	balanceGap float64
	// balanceDwell is the minimum time between balance switches for this
	// pool. Set to defaultBalanceDwell when AQG_POOL_<POOL>_BALANCE_DWELL
	// is absent.
	balanceDwell time.Duration
}

// Load builds a Registry from AQG_POOL_* environment variables, using
// defaultBaseURL (the gateway's ANTHROPIC_BASE_URL) for any pool that
// does not declare its own.
func Load(defaultBaseURL string) (*Registry, error) {
	return loadFrom(os.Environ(), defaultBaseURL)
}

// rawMember is a parsed member declaration before its base URL is
// resolved (the pool default may appear later in the environ scan).
type rawMember struct {
	pool        string
	nick        string
	cred        string
	urlOverride string // "" when the member did not override the pool default
	originKey   string // the env var, for collision/error messages
}

// loadFrom is Load's testable core: it takes "KEY=VALUE" entries in the
// same shape as os.Environ().
//
// It fails closed: a malformed key, an empty credential/nick/pool, a
// collision, a base URL on a pool with no members, a malformed upstream
// URL, or no pools at all are all startup errors rather than a gateway
// that silently can't route. Credential values are never echoed in an
// error.
func loadFrom(environ []string, defaultBaseURL string) (*Registry, error) {
	poolBaseURL := make(map[string]string) // pool -> declared default upstream
	poolURLOrigin := make(map[string]string)
	poolPriority := make(map[string][]string) // pool -> declared preference order
	poolPriorityOrigin := make(map[string]string)
	poolBalance := make(map[string]string)        // pool -> "lead"
	poolBalanceOrigin := make(map[string]string)   // pool -> env key, for errors
	poolBalanceGap := make(map[string]float64)     // pool -> gap fraction
	poolBalanceGapOrigin := make(map[string]string)
	poolBalanceDwell := make(map[string]time.Duration) // pool -> dwell duration
	poolBalanceDwellOrigin := make(map[string]string)
	var members []rawMember
	originKey := make(map[string]string) // pool/nick -> env var that produced it

	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key, val := kv[:eq], kv[eq+1:]
		if !strings.HasPrefix(key, EnvPrefix) {
			continue
		}
		rest := key[len(EnvPrefix):]

		// A member declaration wins over the base-URL shape: a value like
		// AQG_POOL_X_BACKEND_A is always "member A of pool X".
		if idx := strings.Index(rest, backendInfix); idx >= 0 {
			poolName := normalizeName(rest[:idx])
			nick := normalizeName(rest[idx+len(backendInfix):])
			if poolName == "" {
				return nil, fmt.Errorf("backend: %s has an empty pool name", key)
			}
			if nick == "" {
				return nil, fmt.Errorf("backend: %s has an empty nick", key)
			}
			cred, override, err := splitCredURL(val)
			if err != nil {
				return nil, fmt.Errorf("backend: %s %w", key, err)
			}
			qkey := poolName + "/" + nick
			if prev, dup := originKey[qkey]; dup {
				return nil, fmt.Errorf("backend: %s and %s both map to pool %q nick %q", prev, key, poolName, nick)
			}
			originKey[qkey] = key
			members = append(members, rawMember{
				pool:        poolName,
				nick:        nick,
				cred:        cred,
				urlOverride: override,
				originKey:   key,
			})
			continue
		}

		if poolPart, ok := strings.CutSuffix(rest, baseURLSuffix); ok {
			poolName := normalizeName(poolPart)
			if poolName == "" {
				return nil, fmt.Errorf("backend: %s has an empty pool name", key)
			}
			if val == "" {
				return nil, fmt.Errorf("backend: %s has an empty base URL", key)
			}
			if prev, dup := poolURLOrigin[poolName]; dup {
				return nil, fmt.Errorf("backend: %s and %s both set the base URL for pool %q", prev, key, poolName)
			}
			poolURLOrigin[poolName] = key
			poolBaseURL[poolName] = val
			continue
		}

		if poolPart, ok := strings.CutSuffix(rest, prioritySuffix); ok {
			poolName := normalizeName(poolPart)
			if poolName == "" {
				return nil, fmt.Errorf("backend: %s has an empty pool name", key)
			}
			if prev, dup := poolPriorityOrigin[poolName]; dup {
				return nil, fmt.Errorf("backend: %s and %s both set the priority for pool %q", prev, key, poolName)
			}
			order, err := parsePriority(val)
			if err != nil {
				return nil, fmt.Errorf("backend: %s %w", key, err)
			}
			poolPriorityOrigin[poolName] = key
			poolPriority[poolName] = order
			continue
		}

		// _BALANCE_GAP and _BALANCE_DWELL must be checked before _BALANCE
		// because _BALANCE would otherwise match any key ending in _BALANCE
		// (it does not — CutSuffix requires an exact suffix — but the
		// ordering makes the intent clear).
		if poolPart, ok := strings.CutSuffix(rest, balanceGapSuffix); ok {
			poolName := normalizeName(poolPart)
			if poolName == "" {
				return nil, fmt.Errorf("backend: %s has an empty pool name", key)
			}
			if prev, dup := poolBalanceGapOrigin[poolName]; dup {
				return nil, fmt.Errorf("backend: %s and %s both set the balance gap for pool %q", prev, key, poolName)
			}
			f, err := strconv.ParseFloat(val, 64)
			if err != nil || f <= 0 {
				return nil, fmt.Errorf("backend: %s must be a positive fraction (e.g. 0.15)", key)
			}
			poolBalanceGapOrigin[poolName] = key
			poolBalanceGap[poolName] = f
			continue
		}

		if poolPart, ok := strings.CutSuffix(rest, balanceDwellSuffix); ok {
			poolName := normalizeName(poolPart)
			if poolName == "" {
				return nil, fmt.Errorf("backend: %s has an empty pool name", key)
			}
			if prev, dup := poolBalanceDwellOrigin[poolName]; dup {
				return nil, fmt.Errorf("backend: %s and %s both set the balance dwell for pool %q", prev, key, poolName)
			}
			d, err := time.ParseDuration(val)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("backend: %s must be a positive duration (e.g. 5m)", key)
			}
			poolBalanceDwellOrigin[poolName] = key
			poolBalanceDwell[poolName] = d
			continue
		}

		if poolPart, ok := strings.CutSuffix(rest, balanceSuffix); ok {
			poolName := normalizeName(poolPart)
			if poolName == "" {
				return nil, fmt.Errorf("backend: %s has an empty pool name", key)
			}
			if val != "lead" {
				return nil, fmt.Errorf("backend: %s: unsupported balance mode %q; only \"lead\" is supported", key, val)
			}
			if prev, dup := poolBalanceOrigin[poolName]; dup {
				return nil, fmt.Errorf("backend: %s and %s both set the balance mode for pool %q", prev, key, poolName)
			}
			poolBalanceOrigin[poolName] = key
			poolBalance[poolName] = val
			continue
		}

		return nil, fmt.Errorf("backend: %s is not a recognised AQG_POOL_ key (expected suffixes: _BASE_URL, _BACKEND_<NICK>, _PRIORITY, _BALANCE, _BALANCE_GAP, _BALANCE_DWELL)", key)
	}

	if len(members) == 0 {
		return nil, fmt.Errorf("backend: no backends configured; set at least one %s<POOL>%s<NICK>", EnvPrefix, backendInfix)
	}

	pools := make(map[string]*pool)
	for _, m := range members {
		raw := defaultBaseURL
		if u, ok := poolBaseURL[m.pool]; ok {
			raw = u
		}
		if m.urlOverride != "" {
			raw = m.urlOverride
		}
		baseURL, err := validateBaseURL(raw)
		if err != nil {
			return nil, fmt.Errorf("backend: %s has an invalid base URL: %w", m.originKey, err)
		}
		p := pools[m.pool]
		if p == nil {
			p = &pool{name: m.pool, byNick: make(map[string]Backend)}
			pools[m.pool] = p
		}
		p.byNick[m.nick] = Backend{
			Pool:       m.pool,
			Nick:       m.nick,
			Credential: m.cred,
			BaseURL:    baseURL,
		}
	}

	// A base URL declared for a pool with no members is almost certainly a
	// typo'd nick; fail closed rather than silently ignore it.
	for poolName, origin := range poolURLOrigin {
		if _, ok := pools[poolName]; !ok {
			return nil, fmt.Errorf("backend: %s sets a base URL for pool %q, which has no backends", origin, poolName)
		}
	}

	// A priority list must name a real pool and only real members of it.
	// Fail closed on a typo'd pool or nick rather than silently routing by
	// a misspelled preference.
	for poolName, order := range poolPriority {
		p, ok := pools[poolName]
		if !ok {
			return nil, fmt.Errorf("backend: %s sets a priority for pool %q, which has no backends", poolPriorityOrigin[poolName], poolName)
		}
		for _, nick := range order {
			if _, ok := p.byNick[nick]; !ok {
				return nil, fmt.Errorf("backend: %s lists nick %q, which is not a member of pool %q", poolPriorityOrigin[poolName], nick, poolName)
			}
		}
		p.priority = order
	}

	// BALANCE_GAP and BALANCE_DWELL without BALANCE are configuration errors
	// (most likely a misspelling of the pool name or a leftover setting).
	for poolName, origin := range poolBalanceGapOrigin {
		if _, ok := poolBalance[poolName]; !ok {
			return nil, fmt.Errorf("backend: %s sets a balance gap for pool %q but %s%s%s is not set",
				origin, poolName, EnvPrefix, strings.ToUpper(poolName), balanceSuffix)
		}
	}
	for poolName, origin := range poolBalanceDwellOrigin {
		if _, ok := poolBalance[poolName]; !ok {
			return nil, fmt.Errorf("backend: %s sets a balance dwell for pool %q but %s%s%s is not set",
				origin, poolName, EnvPrefix, strings.ToUpper(poolName), balanceSuffix)
		}
	}

	for poolName, mode := range poolBalance {
		p, ok := pools[poolName]
		if !ok {
			return nil, fmt.Errorf("backend: %s sets balance mode for pool %q, which has no backends", poolBalanceOrigin[poolName], poolName)
		}
		if len(p.priority) > 0 {
			return nil, fmt.Errorf("backend: pool %q declares both %s and %s; balanced mode and priority routing are mutually exclusive",
				poolName, poolBalanceOrigin[poolName], poolPriorityOrigin[poolName])
		}
		p.balance = mode
		if gap, ok := poolBalanceGap[poolName]; ok {
			p.balanceGap = gap
		} else {
			p.balanceGap = defaultBalanceGap
		}
		if dwell, ok := poolBalanceDwell[poolName]; ok {
			p.balanceDwell = dwell
		} else {
			p.balanceDwell = defaultBalanceDwell
		}
	}

	for _, p := range pools {
		p.nicks = make([]string, 0, len(p.byNick))
		for nick := range p.byNick {
			p.nicks = append(p.nicks, nick)
		}
		sort.Strings(p.nicks)
	}
	return &Registry{pools: pools}, nil
}

// splitCredURL splits a member value into its credential and an optional
// base-URL override. The override is everything after the first
// separator byte. An empty credential is an error.
func splitCredURL(val string) (cred, override string, err error) {
	cred = val
	if i := strings.IndexByte(val, credURLSep); i >= 0 {
		cred = val[:i]
		override = val[i+1:]
	}
	if cred == "" {
		return "", "", fmt.Errorf("has an empty credential")
	}
	return cred, override, nil
}

// parsePriority parses an AQG_POOL_<POOL>_PRIORITY value into an ordered,
// duplicate-free list of normalized nicks. An empty value, an empty
// entry, or a repeated nick is an error — the membership of each nick is
// checked later, once all members are known. The order is preserved as
// given (highest priority first).
func parsePriority(val string) ([]string, error) {
	if strings.TrimSpace(val) == "" {
		return nil, fmt.Errorf("has an empty priority list")
	}
	parts := strings.Split(val, priorityListSep)
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		nick := normalizeName(p)
		if nick == "" {
			return nil, fmt.Errorf("has an empty nick in its priority list")
		}
		if seen[nick] {
			return nil, fmt.Errorf("lists nick %q more than once in its priority list", nick)
		}
		seen[nick] = true
		out = append(out, nick)
	}
	return out, nil
}

// validateBaseURL enforces that an upstream has a scheme and host, the
// same contract config applies to ANTHROPIC_BASE_URL. The validated
// value is returned unchanged.
func validateBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("scheme and host are required, got %q", raw)
	}
	return raw, nil
}

// HasPool reports whether name (normalized) is a configured pool.
func (r *Registry) HasPool(name string) bool {
	_, ok := r.pools[normalizeName(name)]
	return ok
}

// PoolNames returns the configured pool names in sorted order. Intended
// for startup logging and for building one auto controller per pool — it
// exposes names, never credentials.
func (r *Registry) PoolNames() []string {
	out := make([]string, 0, len(r.pools))
	for name := range r.pools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// PoolNicks returns the member nicks of a pool in sorted order, or nil
// when the pool is unknown.
func (r *Registry) PoolNicks(poolName string) []string {
	p, ok := r.pools[normalizeName(poolName)]
	if !ok {
		return nil
	}
	out := make([]string, len(p.nicks))
	copy(out, p.nicks)
	return out
}

// PoolPriority returns the pool's declared preference order (highest
// first), or nil when the pool is unknown or declared no priority. The
// returned slice is a copy. A non-nil result is the auto controller's
// signal to use priority-ordered selection instead of random/round-robin.
func (r *Registry) PoolPriority(poolName string) []string {
	p, ok := r.pools[normalizeName(poolName)]
	if !ok || len(p.priority) == 0 {
		return nil
	}
	out := make([]string, len(p.priority))
	copy(out, p.priority)
	return out
}

// PoolBalanceGap returns the minimum lead difference the pool requires
// before switching the active member in balanced mode. Returns 0 when the
// pool is not in balanced mode or is unknown — the auto controller treats 0
// as "balance mode off".
func (r *Registry) PoolBalanceGap(poolName string) float64 {
	p, ok := r.pools[normalizeName(poolName)]
	if !ok || p.balance == "" {
		return 0
	}
	return p.balanceGap
}

// PoolBalanceDwell returns the minimum time between balance switches for
// the pool. Returns 0 when the pool is not in balanced mode or is unknown.
func (r *Registry) PoolBalanceDwell(poolName string) time.Duration {
	p, ok := r.pools[normalizeName(poolName)]
	if !ok || p.balance == "" {
		return 0
	}
	return p.balanceDwell
}

// ResolveIn returns the backend named by nick within poolName. ok is
// false when either the pool or the nick is unknown — the caller must
// fail closed rather than fall back.
func (r *Registry) ResolveIn(poolName, nick string) (Backend, bool) {
	p, ok := r.pools[normalizeName(poolName)]
	if !ok {
		return Backend{}, false
	}
	b, ok := p.byNick[normalizeName(nick)]
	return b, ok
}

// NormalizeName canonicalizes a selector the same way the loader
// canonicalizes a pool name, so HTTP-boundary callers that resolve a
// selector (the resolver middleware, the quota endpoint) match the
// configured pool regardless of case or `_`/`-` spelling.
func NormalizeName(raw string) string {
	return normalizeName(raw)
}

// normalizeName canonicalizes a pool name or nick: lowercase, with
// underscores folded to hyphens (so AQG_POOL_Z_AI is addressed as
// "z-ai"), and surrounding hyphens trimmed. The same normalization is
// applied to an inbound selector so the client value matches regardless
// of case.
func normalizeName(raw string) string {
	n := strings.ToLower(strings.TrimSpace(raw))
	n = strings.ReplaceAll(n, "_", "-")
	return strings.Trim(n, "-")
}
