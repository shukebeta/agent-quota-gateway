package persist

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shukebeta/agent-quota-gateway/internal/auto"
)

// TestLoad_missingAddedPoolsIsBackwardCompatible proves a state file written
// before the added_pools field (issue #104) loads cleanly, with AddedPools
// left nil rather than erroring.
func TestLoad_missingAddedPoolsIsBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// A legacy state file: pools + snapshots, no added_pools key.
	legacy := `{"pools":{"auto":{"sticky":"a","exhausted":{}}},"snapshots":{}}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	state, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.AddedPools != nil {
		t.Errorf("AddedPools=%v, want nil for a legacy state file", state.AddedPools)
	}
	if _, ok := state.Pools["auto"]; !ok {
		t.Errorf("legacy pools not loaded: %+v", state.Pools)
	}
}

// TestLoad_roundTripsAddedPools proves added_pools survives a marshal/Load
// round-trip as a set of runtime pool names (post-#172, AddedPoolSpec carries
// no fields — a runtime pool is a pure named marker).
func TestLoad_roundTripsAddedPools(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	data, err := json.Marshal(GatewayState{
		AddedPools: map[string]auto.AddedPoolSpec{
			"rt": {},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := got.AddedPools["rt"]; !ok {
		t.Fatalf("added_pools missing rt after round-trip: %+v", got.AddedPools)
	}
}

// TestLoad_legacyAddedPoolBaseURLIsIgnored proves a pre-#172 state file whose
// added_pools entries still carry a "base_url" field loads cleanly — Go's
// decoder ignores the now-unknown field. No migration, no version bump
// (issue #172 acceptance criterion).
func TestLoad_legacyAddedPoolBaseURLIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	legacy := `{"added_pools":{"legacy":{"base_url":"https://legacy.example"}},"pools":{}}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := got.AddedPools["legacy"]; !ok {
		t.Errorf("legacy runtime pool lost: %+v", got.AddedPools)
	}
}

// TestLoad_missingFileStartsFresh proves a non-existent path is treated as a
// first start: empty state, no error.
func TestLoad_missingFileStartsFresh(t *testing.T) {
	state, err := Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Pools != nil || state.AddedPools != nil {
		t.Errorf("missing file should yield empty state, got %+v", state)
	}
}

// TestLoad_emptyPathStartsFresh proves the disabled-persistence case (empty
// path) returns empty state without touching the filesystem.
func TestLoad_emptyPathStartsFresh(t *testing.T) {
	state, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Pools != nil || state.AddedPools != nil {
		t.Errorf("empty path should yield empty state, got %+v", state)
	}
}

// TestLoad_unparseableStartsFresh proves a corrupt state file logs and starts
// fresh rather than failing startup (the package contract).
func TestLoad_unparseableStartsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	state, err := Load(path)
	if err != nil {
		t.Fatalf("Load should not error on unparseable file: %v", err)
	}
	if state.Pools != nil || state.AddedPools != nil {
		t.Errorf("unparseable file should yield empty state, got %+v", state)
	}
}

// TestLoad_ioErrorIsReturned proves a real read error (path is a directory) is
// surfaced to the caller rather than swallowed.
func TestLoad_ioErrorIsReturned(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("Load on a directory should return an error")
	}
}

// TestNewPersister_emptyPathIsNoOp proves that with persistence disabled the
// Persister never writes: MarkDirty, Run, and a final flush all do nothing and
// no file is created in the working directory.
func TestNewPersister_emptyPathIsNoOp(t *testing.T) {
	called := false
	p := NewPersister("", func() GatewayState { called = true; return GatewayState{} })
	p.MarkDirty()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Run must return immediately on a no-op persister.
	p.Run(ctx)

	if called {
		t.Error("snapFn must not be called when path is empty")
	}
	if _, err := os.Stat(".tmp"); err == nil {
		t.Error("no-op persister wrote a stray .tmp file")
	}
}

// TestMarkDirty_coalesces proves repeated MarkDirty calls never block and
// collapse to a single pending signal (the buffered cap-1 dirty channel).
func TestMarkDirty_coalesces(t *testing.T) {
	p := NewPersister(filepath.Join(t.TempDir(), "state.json"), func() GatewayState { return GatewayState{} })
	for i := 0; i < 100; i++ {
		p.MarkDirty() // must not block even with no Run draining.
	}
	if got := len(p.dirty); got != 1 {
		t.Errorf("pending signals = %d, want 1 (coalesced)", got)
	}
}

// stateWith returns a GatewayState carrying an identifiable added pool so a
// flushed file can be distinguished from empty. The marker is the presence of
// the "rt" key — post-#172 the AddedPoolSpec carries no fields.
func stateWith(_ string) GatewayState {
	return GatewayState{AddedPools: map[string]auto.AddedPoolSpec{"rt": {}}}
}

// waitForFile polls for path to appear, failing the test if it never does.
// Polling (rather than an exact debounce-window Sleep) keeps the test stable
// under CI load.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state file %q never appeared", path)
}

// TestRun_flushesAfterDebounce proves the debounce loop writes a marked-dirty
// state to disk once the window elapses.
func TestRun_flushesAfterDebounce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p := NewPersister(path, func() GatewayState { return stateWith("https://debounce.example") })
	p.debounce = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	p.MarkDirty()
	waitForFile(t, path)

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := got.AddedPools["rt"]; !ok {
		t.Errorf("flushed state missing rt added pool: %+v", got.AddedPools)
	}
}

// TestRun_finalFlushOnShutdown proves a pending mutation is persisted when the
// context is cancelled, even though the debounce timer has not fired. A long
// debounce ensures the only path that can write is the shutdown flush.
func TestRun_finalFlushOnShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p := NewPersister(path, func() GatewayState { return stateWith("https://shutdown.example") })
	p.debounce = 30 * time.Second // long enough that the timer never fires in this test

	ctx, cancel := context.WithCancel(context.Background())
	go p.Run(ctx)

	p.MarkDirty()
	// Wait until Run has consumed the dirty signal (and thus set pending),
	// so the subsequent cancel deterministically takes the final-flush path.
	for i := 0; i < 400 && len(p.dirty) != 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	waitForFile(t, path)

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := got.AddedPools["rt"]; !ok {
		t.Errorf("final-flush state missing rt added pool: %+v", got.AddedPools)
	}
}

// TestFlush_atomicAnd0600 proves flush writes at mode 0600 and leaves no
// leftover temp file after the rename.
func TestFlush_atomicAnd0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	p := NewPersister(path, func() GatewayState { return stateWith("https://flush.example") })
	p.flush()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("state file mode = %o, want 600", perm)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("flush left a stray .tmp file behind")
	}
}
