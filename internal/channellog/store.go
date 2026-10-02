package channellog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The facts are appended as JSONL, one file per UTC day of the request:
//
//	channel-log-2026-10-02.jsonl
//
// The naming matches the observation store's own convention (channel-<date>.jsonl) with a
// distinct prefix on purpose: both files live in the same directory, and the observation
// store's reader lists "channel-*.jsonl" and skips whatever it cannot read as a date. A
// distinct prefix makes that skip a decision rather than an accident.
//
// One file per day is what makes every later read cheap. Writes are one open/append/close per
// fact: the scanner is off the request path, the volume is one line per request, and closing
// the handle is what makes "the fact persisted" true before the log file is unlinked.
const (
	factPrefix   = "channel-log-"
	factSuffix   = ".jsonl"
	dateLayout   = "2006-01-02"
	directoryFor = 0o755
	fileFor      = 0o644
)

type factWriter struct {
	dir string
}

// append writes one fact. It is fail-open by contract: the scanner turns the error into a
// health counter and keeps the log file, because a fact that did not reach the disk must not
// be the reason a source file disappears.
func (w *factWriter) append(fact Fact) error {
	if w == nil || w.dir == "" {
		return errors.New("no fact directory configured")
	}
	encoded, errEncode := json.Marshal(fact)
	if errEncode != nil {
		return fmt.Errorf("encode fact: %w", errEncode)
	}
	encoded = append(encoded, '\n')

	day := fact.Time
	if day.IsZero() {
		day = fact.ParsedAt
	}
	if day.IsZero() {
		day = time.Now()
	}
	if errMkdir := os.MkdirAll(w.dir, directoryFor); errMkdir != nil {
		return fmt.Errorf("create fact directory: %w", errMkdir)
	}
	path := filepath.Join(w.dir, factPrefix+day.UTC().Format(dateLayout)+factSuffix)
	handle, errOpen := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileFor)
	if errOpen != nil {
		return fmt.Errorf("open fact file: %w", errOpen)
	}
	if _, errWrite := handle.Write(encoded); errWrite != nil {
		_ = handle.Close()
		return fmt.Errorf("append fact: %w", errWrite)
	}
	if errClose := handle.Close(); errClose != nil {
		return fmt.Errorf("close fact file: %w", errClose)
	}
	return nil
}
