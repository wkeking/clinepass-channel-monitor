package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// InstanceFilePrefix names the per-runner heartbeat files inside the guard directory.
//
// CPA replaces a plugin by file name and keeps the previous .so loaded until the process exits
// (internal/pluginhost/host.go, retireLoadedPluginLocked), so more than one build of this plugin
// can be live at the same time - each with its own guard, each judging the same accounts from the
// configuration it was loaded with. The heartbeat is what makes that visible: every runner touches
// its own file and /health reports how many of them are alive.
//
// Measured on the production host on 2026-10-13: three instances were loaded (0.4.0, 0.3.1 and
// 0.4.1) and nothing the plugin published showed it.
const InstanceFilePrefix = "account-guard.instance-"

// instanceFileSuffix keeps the heartbeat files identifiable when a human lists the directory.
const instanceFileSuffix = ".json"

// instanceStaleAfter is how quiet a heartbeat has to be before another runner treats it as dead
// and removes it. It is a multiple of the runner's own save interval (15s), so a slow tick can
// never make a live instance look dead - and it is the only bound on how long a file from a
// crashed process stays behind.
const instanceStaleAfter = 5 * time.Minute

// InstanceInfo is one live guard runner, as written by its heartbeat file.
type InstanceInfo struct {
	// ID is random per runner, so two runners in the same process never share a file.
	ID string `json:"id"`
	// Version is the plugin build that runner belongs to.
	Version string `json:"version,omitempty"`
	// StartedAt is when that runner applied its configuration.
	StartedAt time.Time `json:"started_at"`
	// LastSeen is refreshed on every heartbeat.
	LastSeen time.Time `json:"last_seen"`
	// PID is the host process. Every instance of one host shares it.
	PID int `json:"pid,omitempty"`
}

// TouchInstance writes one runner's heartbeat. A heartbeat is a diagnostic and never an input to
// a decision, so it is written without the state file's atomic-replace dance: a torn read of a
// heartbeat is caught by the decoder and skipped.
func (s *Store) TouchInstance(info InstanceInfo) error {
	if s == nil || strings.TrimSpace(info.ID) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.dirPath()
	if errMkdir := os.MkdirAll(dir, dirMode); errMkdir != nil {
		return fmt.Errorf("create account guard directory: %w", errMkdir)
	}
	data, errMarshal := json.Marshal(info)
	if errMarshal != nil {
		return fmt.Errorf("encode account guard heartbeat: %w", errMarshal)
	}
	data = append(data, '\n')
	path := filepath.Join(dir, InstanceFilePrefix+info.ID+instanceFileSuffix)
	if errWrite := os.WriteFile(path, data, stateFileMode); errWrite != nil {
		return fmt.Errorf("write account guard heartbeat: %w", errWrite)
	}
	return nil
}

// LiveInstances returns the heartbeats refreshed within the window, oldest start first, and
// removes the ones that have been quiet for instanceStaleAfter.
//
// It is deliberately forgiving: a directory that cannot be listed, a file that cannot be read and
// a line that cannot be decoded are all skipped, because "how many guards are running" is a
// warning, not a fact the guard may act on.
func (s *Store) LiveInstances(within time.Duration) ([]InstanceInfo, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.dirPath()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		if errors.Is(errRead, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read account guard directory: %w", errRead)
	}
	now := time.Now().UTC()
	out := make([]InstanceInfo, 0, 2)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, InstanceFilePrefix) || !strings.HasSuffix(name, instanceFileSuffix) {
			continue
		}
		path := filepath.Join(dir, name)
		data, errFile := os.ReadFile(path)
		if errFile != nil {
			continue
		}
		var info InstanceInfo
		if errDecode := json.Unmarshal(data, &info); errDecode != nil {
			continue
		}
		age := now.Sub(info.LastSeen.UTC())
		if age > instanceStaleAfter {
			_ = os.Remove(path)
			continue
		}
		if within > 0 && age > within {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(first, second int) bool {
		return out[first].StartedAt.Before(out[second].StartedAt)
	})
	return out, nil
}
