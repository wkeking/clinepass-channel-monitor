package guard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// marshalState renders a state the way the store would, for round-trip comparisons.
func marshalState(t *testing.T, state *State) string {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	return string(data)
}

// TestStoreLoadMissingFile covers the "no file yet" contract.
func TestStoreLoadMissingFile(t *testing.T) {
	state, err := NewStore(t.TempDir()).Load()
	if err != nil {
		t.Fatalf("Load on a missing file returned %v", err)
	}
	if state == nil || state.Accounts == nil {
		t.Fatalf("Load returned %+v, want a fresh state", state)
	}
}

// TestStoreSaveLoadRoundTrip covers state persistence and identical behavior after a
// reload.
func TestStoreSaveLoadRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir())
	accounts := []Account{acct("Cline1", cline1Key, false), witness()}

	state := NewState()
	state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
	}, accounts, at(2), judgeOpts())
	if err := store.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := marshalState(t, loaded), marshalState(t, state); got != want {
		t.Fatalf("round trip changed the state:\n got %s\nwant %s", got, want)
	}

	decisions := loaded.Advance([]Sample{smp(2, "r3", cline1Key, "fireworks")}, accounts, at(3), judgeOpts())
	if len(decisions) != 1 || decisions[0].Action != ActionDisable || decisions[0].Streak != 3 {
		t.Fatalf("decision after reload = %+v, want one disable at streak 3", decisions)
	}
}

// TestStoreCrossRestartNoDoubleCount covers the restart-safe deduplication: feeding the
// same batch again must not count twice or emit a decision.
func TestStoreCrossRestartNoDoubleCount(t *testing.T) {
	store := NewStore(t.TempDir())
	accounts := []Account{acct("Cline1", cline1Key, false), witness()}
	batch := []Sample{smp(0, "r1", cline1Key, "fireworks")}

	state := NewState()
	if decisions := state.Advance(batch, accounts, at(1), judgeOpts()); len(decisions) != 0 {
		t.Fatalf("first batch produced %+v", decisions)
	}
	if err := store.Save(state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if decisions := loaded.Advance(batch, accounts, at(2), judgeOpts()); len(decisions) != 0 {
		t.Fatalf("replayed batch produced %+v", decisions)
	}
	if got := slotFor(t, loaded, cline1Key).Streak; got != 1 {
		t.Fatalf("streak after a replay = %d, want 1", got)
	}

	decisions := loaded.Advance([]Sample{
		smp(2, "r2", cline1Key, "fireworks"),
		smp(3, "r3", cline1Key, "fireworks"),
	}, accounts, at(4), judgeOpts())
	if len(decisions) != 1 || decisions[0].Streak != 3 {
		t.Fatalf("decisions = %+v, want one at streak 3", decisions)
	}
}

// TestStoreLoadCorrupted covers a damaged state file: an error, never a panic.
func TestStoreLoadCorrupted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, StateFileName)
	if err := os.WriteFile(path, []byte("{ this is not json"), stateFileMode); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	state, err := NewStore(dir).Load()
	if err == nil {
		t.Fatalf("Load on corrupted JSON returned state %+v and no error", state)
	}
	if state != nil {
		t.Fatalf("Load returned a state alongside the error: %+v", state)
	}
}

// TestStoreSaveIsAtomic covers the file contract: private mode, complete JSON and no
// leftover temp files after a replace.
func TestStoreSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	path := filepath.Join(dir, StateFileName)

	if err := store.Save(NewState()); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	state := NewState()
	state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false), witness()}, at(2), judgeOpts())
	if err := store.Save(state); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("state file is not complete JSON: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state: %v", err)
	}
	if got := info.Mode().Perm(); got != stateFileMode {
		t.Fatalf("state file mode = %o, want %o", got, stateFileMode)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, StateFileName+".*.tmp"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("Save left temp files behind: %v", leftovers)
	}
}

// TestStoreAppendAuditLines covers the audit trail: appended, not overwritten, and one
// parseable JSON document per line.
func TestStoreAppendAuditLines(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	want := []Decision{
		{Time: at(0), Name: "Cline1", ProviderKey: cline1Key, Action: ActionDisable, Streak: 3, Baseline: "deepseek"},
		{Time: at(60), Name: "Cline1", ProviderKey: cline1Key, Action: ActionEnable, Streak: 0, Baseline: "deepseek"},
		{Time: at(120), Name: "Cline2", ProviderKey: cline2Key, Action: ActionDisableDry, Streak: 3, Baseline: "deepseek"},
	}
	for _, decision := range want {
		if err := store.AppendAudit(decision); err != nil {
			t.Fatalf("AppendAudit: %v", err)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, AuditFileName))
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != len(want) {
		t.Fatalf("audit has %d lines, want %d", len(lines), len(want))
	}
	for i, line := range lines {
		var got Decision
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is not JSON: %v", i, err)
		}
		if got.Action != want[i].Action || got.Name != want[i].Name || got.Streak != want[i].Streak {
			t.Fatalf("line %d = %+v, want %+v", i, got, want[i])
		}
	}
}
