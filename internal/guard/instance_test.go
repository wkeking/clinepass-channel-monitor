package guard

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveInstancesReportsEveryRunningGuard pins the diagnostic the 2026-10-13 incident was missing:
// CPA replaced the plugin twice and kept both retired .so files loaded, so three guards were judging
// the same accounts - each from the configuration it was loaded with - and nothing the plugin
// published showed it. One file per runner, oldest start first.
func TestLiveInstancesReportsEveryRunningGuard(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	now := time.Now().UTC()
	older := InstanceInfo{
		ID: "aaa", Version: "0.4.0", StartedAt: now.Add(-2 * time.Hour),
		LastSeen: now.Add(-time.Minute), PID: 42,
	}
	newer := InstanceInfo{
		ID: "bbb", Version: "0.4.1", StartedAt: now.Add(-time.Minute),
		LastSeen: now, PID: 42,
	}
	for _, info := range []InstanceInfo{newer, older} {
		if errTouch := store.TouchInstance(info); errTouch != nil {
			t.Fatalf("touch %s: %v", info.ID, errTouch)
		}
	}

	live, errLive := store.LiveInstances(instanceStaleAfter)
	if errLive != nil {
		t.Fatalf("live instances: %v", errLive)
	}
	if len(live) != 2 {
		t.Fatalf("live = %#v, want two instances", live)
	}
	if live[0].ID != "aaa" || live[0].Version != "0.4.0" {
		t.Errorf("live[0] = %#v, want the oldest start first", live[0])
	}
	if live[1].ID != "bbb" || live[1].Version != "0.4.1" {
		t.Errorf("live[1] = %#v", live[1])
	}
}

// TestLiveInstancesForgetsAHeartbeatThatStopped covers the other half: a process that dies leaves
// its file behind, and a file nobody refreshes must not keep inflating the instance count. The
// heartbeat window the guard uses is short (a few save intervals); the staleness bound is what
// eventually removes the file.
func TestLiveInstancesForgetsAHeartbeatThatStopped(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	now := time.Now().UTC()
	stale := InstanceInfo{
		ID: "gone", Version: "0.3.1", StartedAt: now.Add(-time.Hour),
		LastSeen: now.Add(-instanceStaleAfter - time.Minute),
	}
	quiet := InstanceInfo{
		ID: "quiet", Version: "0.4.1", StartedAt: now.Add(-time.Hour),
		LastSeen: now.Add(-2 * time.Minute),
	}
	for _, info := range []InstanceInfo{stale, quiet} {
		if errTouch := store.TouchInstance(info); errTouch != nil {
			t.Fatalf("touch %s: %v", info.ID, errTouch)
		}
	}

	live, errLive := store.LiveInstances(60 * time.Second)
	if errLive != nil {
		t.Fatalf("live instances: %v", errLive)
	}
	if len(live) != 0 {
		t.Errorf("live = %#v, want nothing inside a 60s heartbeat window", live)
	}
	if _, errStat := os.Stat(filepath.Join(dir, InstanceFilePrefix+"gone"+instanceFileSuffix)); !os.IsNotExist(errStat) {
		t.Errorf("a stale heartbeat must be removed, stat error = %v", errStat)
	}
	if _, errStat := os.Stat(filepath.Join(dir, InstanceFilePrefix+"quiet"+instanceFileSuffix)); errStat != nil {
		t.Errorf("a heartbeat inside the staleness bound must stay on disk: %v", errStat)
	}
}

// TestLiveInstancesIgnoresForeignAndTornFiles pins the tolerant half: the guard directory also holds
// the state file, the audit log and a year of channel records, and a heartbeat caught mid-write must
// never turn into an error or a phantom instance.
func TestLiveInstancesIgnoresForeignAndTornFiles(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if errWrite := os.WriteFile(filepath.Join(dir, StateFileName), []byte("{}\n"), stateFileMode); errWrite != nil {
		t.Fatalf("write the state file: %v", errWrite)
	}
	if errWrite := os.WriteFile(filepath.Join(dir, InstanceFilePrefix+"torn"+instanceFileSuffix), []byte("{not json"), stateFileMode); errWrite != nil {
		t.Fatalf("write a torn heartbeat: %v", errWrite)
	}

	live, errLive := store.LiveInstances(0)
	if errLive != nil {
		t.Fatalf("live instances: %v", errLive)
	}
	if len(live) != 0 {
		t.Errorf("live = %#v, want no phantom instance", live)
	}
}

// TestTouchInstanceNeedsAnID pins the no-op case: a runner without an identity writes no file, so a
// half-built runner can never claim a heartbeat.
func TestTouchInstanceNeedsAnID(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if errTouch := store.TouchInstance(InstanceInfo{Version: "0.4.1"}); errTouch != nil {
		t.Fatalf("touch without an id: %v", errTouch)
	}
	matches, errGlob := filepath.Glob(filepath.Join(dir, InstanceFilePrefix+"*"+instanceFileSuffix))
	if errGlob != nil {
		t.Fatalf("glob: %v", errGlob)
	}
	if len(matches) != 0 {
		t.Errorf("heartbeat files = %v, want none", matches)
	}
}
