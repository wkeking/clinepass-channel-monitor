package guard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// File names owned by the guard inside the caller-supplied directory.
const (
	// StateFileName holds the JSON state, replaced atomically on every Save.
	StateFileName = "account-guard.json"
	// AuditFileName holds one JSON Decision per line, appended and fsynced.
	AuditFileName = "account-guard.jsonl"
)

// File modes: the state and audit files are private to the plugin user.
const (
	stateFileMode = 0o600
	dirMode       = 0o700
)

// Store persists the guard state and its audit trail under one directory. Its methods
// are safe for concurrent use.
type Store struct {
	mu  sync.Mutex
	dir string
}

// NewStore returns a store rooted at dir. An empty dir means the working directory.
func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

// Load reads the state file. A missing file is not an error: it returns a fresh state.
// Corrupted JSON returns an error instead of panicking, so the caller can decide
// whether to start over.
func (s *Store) Load() (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.statePath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewState(), nil
		}
		return nil, fmt.Errorf("read account guard state: %w", err)
	}

	state := NewState()
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, state); err != nil {
			return nil, fmt.Errorf("decode account guard state: %w", err)
		}
	}
	state.hydrate()
	return state, nil
}

// Save writes the state through a temporary file and an atomic rename, so a reader
// never sees a half-written document and a crash between the two leaves the previous
// file intact.
func (s *Store) Save(state *State) error {
	if state == nil {
		return errors.New("save account guard state: nil state")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.dirPath()
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create account guard directory: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode account guard state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, StateFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create account guard temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Remove the temp file unless the rename below succeeds.
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(stateFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set account guard temp mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write account guard temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync account guard temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close account guard temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.statePath()); err != nil {
		return fmt.Errorf("replace account guard state: %w", err)
	}
	renamed = true
	syncDir(dir)
	return nil
}

// RecentAudit returns the last limit decisions from the audit file, oldest first, and nil when
// there are none. Unparseable lines are skipped rather than failing the read: the file is appended
// to by a live process, and one torn line must not hide the rest of the history.
//
// This is the durable record of what the guard decided. The in-memory list a runner keeps is only
// a cache of it, and that cache is empty in every process that did not personally make the
// decision - which is exactly what a plugin reload leaves behind.
func (s *Store) RecentAudit(limit int) ([]Decision, error) {
	if limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	file, errOpen := os.Open(s.auditPath())
	if errOpen != nil {
		if errors.Is(errOpen, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read account guard audit: %w", errOpen)
	}
	defer func() { _ = file.Close() }()

	ring := make([]Decision, 0, limit)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var decision Decision
		if errDecode := json.Unmarshal(line, &decision); errDecode != nil {
			continue
		}
		if len(ring) == limit {
			ring = append(ring[:0], ring[1:]...)
		}
		ring = append(ring, decision)
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, fmt.Errorf("read account guard audit: %w", errScan)
	}
	if len(ring) == 0 {
		return nil, nil
	}
	return ring, nil
}

// AppendAudit appends one decision as a JSON line and fsyncs it.
func (s *Store) AppendAudit(decision Decision) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.dirPath()
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create account guard directory: %w", err)
	}

	line, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("encode account guard audit: %w", err)
	}
	line = append(line, '\n')

	file, err := os.OpenFile(s.auditPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, stateFileMode)
	if err != nil {
		return fmt.Errorf("open account guard audit: %w", err)
	}
	defer func() { _ = file.Close() }()

	if _, err := file.Write(line); err != nil {
		return fmt.Errorf("append account guard audit: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync account guard audit: %w", err)
	}
	return nil
}

// statePath is the absolute-or-relative path of the state file.
func (s *Store) statePath() string {
	return filepath.Join(s.dirPath(), StateFileName)
}

// auditPath is the absolute-or-relative path of the audit file.
func (s *Store) auditPath() string {
	return filepath.Join(s.dirPath(), AuditFileName)
}

// dirPath maps an empty directory to the working directory.
func (s *Store) dirPath() string {
	if s.dir == "" {
		return "."
	}
	return s.dir
}

// syncDir fsyncs the directory so the rename is durable. Some filesystems reject a
// directory sync; that must not fail a save whose data is already in place.
func syncDir(dir string) {
	handle, err := os.Open(dir)
	if err != nil {
		return
	}
	defer func() { _ = handle.Close() }()
	_ = handle.Sync()
}
