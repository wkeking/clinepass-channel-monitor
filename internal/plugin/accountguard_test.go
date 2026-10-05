package plugin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/guard"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostconf"
	"github.com/wkeking/clinepass-channel-monitor/internal/observation"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// guardFixture is a CPA configuration with the v8 layout and one enabled Cline account.
const guardFixture = `plugins:
    enabled: true
api-keys:
    openai-compatibility:
        - "base-url": "https://api.cline.bot/api/v1"
          "disabled": false
          "keys":
            - "api-key": "sk-FIXTURE-cline1"
          "models":
            - "alias": "deepseek-flash-1"
              "name": "cline-pass/deepseek-v4.1-flash"
          "name": "Cline1"
`

func writeGuardConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(path, []byte(body), 0o600); errWrite != nil {
		t.Fatalf("write configuration: %v", errWrite)
	}
	return path
}

func newGuardRunner(t *testing.T, path string) *accountGuardRunner {
	t.Helper()
	dir := t.TempDir()
	return &accountGuardRunner{
		cfg:     config.Config{PlanConfigPath: path, ChannelStoreDir: dir, AccountGuardThreshold: 3},
		store:   guard.NewStore(dir),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		state:   guard.NewState(),
		pending: map[string]bool{},
	}
}

// TestAccountGuardApplyFlipsOneEntryAndRecordsIt pins the one write this plugin performs on
// CPA's own configuration: the token changes, the state and audit files exist, and nothing is
// left pending once the file reads back.
func TestAccountGuardApplyFlipsOneEntryAndRecordsIt(t *testing.T) {
	path := writeGuardConfig(t, guardFixture)
	runner := newGuardRunner(t, path)
	decision := guard.Decision{
		Time: time.Now().UTC(), Name: "Cline1", ProviderKey: "openai-compatible-cline1",
		Action: "disable", Streak: 3, Reason: "Cline1 连续 3 次非基准渠道（deepseek）",
	}
	runner.record(decision)
	runner.apply(decision)

	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read back: %v", errRead)
	}
	if !strings.Contains(string(raw), `"disabled": true`) {
		t.Fatalf("the entry was not disabled:\n%s", raw)
	}
	entries := hostconf.Entries(raw)
	if len(entries) != 1 || !entries[0].Disabled {
		t.Fatalf("entries = %#v, want Cline1 disabled", entries)
	}
	if len(runner.pending) != 0 {
		t.Errorf("pending = %v, want nothing pending after a successful write", runner.pending)
	}
	audit, errAudit := os.ReadFile(filepath.Join(runner.cfg.ChannelStoreDir, "account-guard.jsonl"))
	if errAudit != nil {
		t.Fatalf("audit file: %v", errAudit)
	}
	if !strings.Contains(string(audit), `"action":"disable"`) {
		t.Errorf("audit line = %s", audit)
	}
	if _, errState := os.Stat(filepath.Join(runner.cfg.ChannelStoreDir, "account-guard.json")); errState != nil {
		t.Errorf("state file: %v", errState)
	}
}

// TestAccountGuardApplyKeepsTheValuePendingWhenTheWriteCannotLand pins the failure path: a
// decision that did not reach the file must keep being shown as wanted, otherwise the next tick
// would read "still enabled" and mistake it for the operator re-enabling the account.
func TestAccountGuardApplyKeepsTheValuePendingWhenTheWriteCannotLand(t *testing.T) {
	// A directory reads as a configuration file that cannot be read back, so the write path
	// fails before it can touch anything.
	runner := newGuardRunner(t, t.TempDir())
	decision := guard.Decision{
		Time: time.Now().UTC(), Name: "Cline1", ProviderKey: "openai-compatible-cline1",
		Action: "disable", Streak: 3, Reason: "test",
	}
	runner.apply(decision)
	if got, ok := runner.pending["openai-compatible-cline1"]; !ok || !got {
		t.Fatalf("pending = %v, want the disable kept pending", runner.pending)
	}
}

// TestAccountGuardApplyLeavesAMatchingFileAlone pins that a decision the file already agrees with
// writes nothing at all.
func TestAccountGuardApplyLeavesAMatchingFileAlone(t *testing.T) {
	path := writeGuardConfig(t, strings.Replace(guardFixture, `"disabled": false`, `"disabled": true`, 1))
	runner := newGuardRunner(t, path)
	before, errBefore := os.ReadFile(path)
	if errBefore != nil {
		t.Fatalf("read: %v", errBefore)
	}
	runner.apply(guard.Decision{
		Time: time.Now().UTC(), Name: "Cline1", ProviderKey: "openai-compatible-cline1",
		Action: "disable", Streak: 3, Reason: "test",
	})
	after, errAfter := os.ReadFile(path)
	if errAfter != nil {
		t.Fatalf("read back: %v", errAfter)
	}
	if string(before) != string(after) {
		t.Error("a decision the file already satisfies must not rewrite it")
	}
}

// TestAccountGuardAccountsHonourPending pins what the state machine is told while a write is
// still in flight.
func TestAccountGuardAccountsHonourPending(t *testing.T) {
	path := writeGuardConfig(t, guardFixture)
	runner := newGuardRunner(t, path)
	raw, _ := os.ReadFile(path)
	accounts := runner.accounts(raw)
	if len(accounts) != 1 || accounts[0].Disabled {
		t.Fatalf("accounts = %#v, want one enabled account", accounts)
	}
	runner.pending["openai-compatible-cline1"] = true
	accounts = runner.accounts(raw)
	if len(accounts) != 1 || !accounts[0].Disabled {
		t.Fatalf("accounts = %#v, want the pending disable applied", accounts)
	}
}

// TestAccountGuardLifecyclePublishesASnapshot pins start/stop: a runner that is switched off still
// publishes a snapshot (so the page can say "off"), and stopping unpublishes it.
func TestAccountGuardLifecyclePublishesASnapshot(t *testing.T) {
	path := writeGuardConfig(t, guardFixture)
	cfg := config.Config{PlanConfigPath: path, ChannelStoreDir: t.TempDir(), AccountGuardEnabled: false}
	startAccountGuard(cfg)
	published := state.AccountGuard()
	if published == nil {
		t.Fatal("startAccountGuard must publish a snapshot synchronously")
	}
	if published.Enabled || published.Entries != 1 {
		t.Fatalf("snapshot = %#v, want the guard off with one account read", published)
	}
	stopAccountGuard()
	if state.AccountGuard() != nil {
		t.Error("stopAccountGuard must unpublish the snapshot")
	}
}

// TestWriteConfigAtomicKeepsModeAndCleansUp pins the two properties that keep a botched write
// from taking the gateway down: the replacement is a rename of a temp file, and it keeps the
// file's mode.
func TestWriteConfigAtomicKeepsModeAndCleansUp(t *testing.T) {
	path := writeGuardConfig(t, guardFixture)
	if errChmod := os.Chmod(path, 0o640); errChmod != nil {
		t.Fatalf("chmod: %v", errChmod)
	}
	if errWrite := writeConfigAtomic(path, []byte("hello: world\n")); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat: %v", errStat)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640 preserved", info.Mode().Perm())
	}
	leftovers, errGlob := filepath.Glob(filepath.Join(filepath.Dir(path), ".clinepass-*.tmp"))
	if errGlob != nil {
		t.Fatalf("glob: %v", errGlob)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// TestWriteConfigFallsBackToAnInPlaceWrite covers the production deployment: config.yaml is
// bind-mounted into the container, so renaming a temp file over it fails with EBUSY. Measured on
// the live host on 2026-10-05 - the account guard's disable and the defaults seeder both hit it.
// The bytes still have to land, at the file's own permissions, with no temp file left behind.
func TestWriteConfigFallsBackToAnInPlaceWrite(t *testing.T) {
	path := writeGuardConfig(t, "old: value\n")
	if errChmod := os.Chmod(path, 0o640); errChmod != nil {
		t.Fatalf("chmod: %v", errChmod)
	}
	restore := renameFile
	renameFile = func(string, string) error { return errors.New("device or resource busy") }
	defer func() { renameFile = restore }()

	if errWrite := writeConfigAtomic(path, []byte("new: value\n")); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	got, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read: %v", errRead)
	}
	if string(got) != "new: value\n" {
		t.Errorf("content = %q, want the bytes to land in place", got)
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat: %v", errStat)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640 preserved", info.Mode().Perm())
	}
	leftovers, errGlob := filepath.Glob(filepath.Join(filepath.Dir(path), ".clinepass-*.tmp"))
	if errGlob != nil {
		t.Fatalf("glob: %v", errGlob)
	}
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// TestGuardSamplesMapAndSortTheRecords pins the one place where the page's records become the
// state machine's input: the credential, the joined channel (empty when there was none) and the
// request identity, oldest first, with credential-less records dropped.
func TestGuardSamplesMapAndSortTheRecords(t *testing.T) {
	base := time.Date(2026, 10, 5, 4, 0, 0, 0, time.UTC)
	records := []observation.Record{
		{Time: base.Add(2 * time.Minute), RequestID: "req-3", CPProvider: "openai-compatible-cline1", FinalProvider: "deepseek"},
		{Time: base, RequestID: "req-1", CPProvider: "openai-compatible-cline1", FinalProvider: "fireworks"},
		{Time: base.Add(time.Minute), RequestID: "req-2"},
		{Time: base.Add(3 * time.Minute), RequestID: "req-4", CPProvider: "openai-compatible-cline2"},
	}
	samples := guardSamples(records)
	if len(samples) != 3 {
		t.Fatalf("samples = %#v, want three (the credential-less record is dropped)", samples)
	}
	if samples[0].RequestID != "req-1" || samples[1].RequestID != "req-3" || samples[2].RequestID != "req-4" {
		t.Fatalf("order = %q, %q, %q, want oldest first", samples[0].RequestID, samples[1].RequestID, samples[2].RequestID)
	}
	if samples[0].ProviderKey != "openai-compatible-cline1" || samples[0].Channel != "fireworks" {
		t.Errorf("sample 0 = %#v", samples[0])
	}
	if samples[2].Channel != "" {
		t.Errorf("sample 2 channel = %q, want empty for a record with no channel block", samples[2].Channel)
	}
}
