package observation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/channellog"
)

// TestStoreFactSourceReadsPersistedFacts pins the reason this source exists: the scanner's ring
// is empty after a restart, so a window can only be answered from the facts on disk.
func TestStoreFactSourceReadsPersistedFacts(t *testing.T) {
	dir := t.TempDir()
	stamp := time.Date(2026, 10, 2, 15, 1, 20, 0, time.UTC)
	fact := channellog.Fact{
		Time:             stamp,
		SessionID:        "session-abc",
		SessionUUID:      "abc",
		HasSession:       true,
		FinalProvider:    "deepseek",
		ResolvedProvider: "deepseek",
		SourceFile:       "v1-responses-2026-10-02T230120-x.log",
	}
	raw, errMarshal := json.Marshal(fact)
	if errMarshal != nil {
		t.Fatalf("marshal fact: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(dir, "channel-log-2026-10-02.jsonl"), append(raw, '\n'), 0o644); errWrite != nil {
		t.Fatalf("write fact file: %v", errWrite)
	}
	// A record file in the same directory must never be parsed as a fact.
	if errWrite := os.WriteFile(filepath.Join(dir, "channel-2026-10-02.jsonl"), []byte("{\"v\":3}\n"), 0o644); errWrite != nil {
		t.Fatalf("write record file: %v", errWrite)
	}

	source := FactsFromStoreAndScanner(dir, nil)
	facts := source.Facts()
	if len(facts) != 1 {
		t.Fatalf("facts = %d, want exactly the one persisted fact", len(facts))
	}
	if facts[0].FinalProvider != "deepseek" || facts[0].SessionUUID != "abc" {
		t.Errorf("fact = %+v, want the persisted channel and session", facts[0])
	}
	if !facts[0].Time.Equal(stamp) {
		t.Errorf("fact time = %v, want %v", facts[0].Time, stamp)
	}
}

// TestStoreFactSourceMergesTheRingOnce keeps the two halves from double-counting the same
// request: a fact the ring holds and the file holds is one request.
func TestStoreFactSourceMergesTheRingOnce(t *testing.T) {
	dir := t.TempDir()
	stamp := time.Date(2026, 10, 2, 15, 1, 20, 0, time.UTC)
	shared := channellog.Fact{Time: stamp, FinalProvider: "deepseek", SourceFile: "req-1.log"}
	onlyRing := channellog.Fact{Time: stamp.Add(time.Minute), FinalProvider: "deepseek", SourceFile: "req-2.log"}
	raw, _ := json.Marshal(shared)
	if errWrite := os.WriteFile(filepath.Join(dir, "channel-log-2026-10-02.jsonl"), append(raw, '\n'), 0o644); errWrite != nil {
		t.Fatalf("write fact file: %v", errWrite)
	}
	ring := FactSourceFunc(func() []channellog.Fact {
		return []channellog.Fact{shared, onlyRing}
	})

	facts := FactsFromStoreAndScanner(dir, ring).Facts()
	if len(facts) != 2 {
		t.Fatalf("facts = %d, want the shared fact once plus the ring-only fact", len(facts))
	}
	if !facts[0].Time.After(facts[1].Time) {
		t.Errorf("facts must be newest first: %v", []time.Time{facts[0].Time, facts[1].Time})
	}
}
