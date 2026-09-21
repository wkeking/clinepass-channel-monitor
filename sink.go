package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	jsonlFilePrefix = "channel-monitor-"
	jsonlFileSuffix = ".jsonl"
	jsonlFileMode   = 0o644
	jsonlDirMode    = 0o755
	cleanupInterval = time.Hour
	sinkQueueSize   = 1024
	sinkFlushEvery  = 2 * time.Second
)

// jsonlWriter appends one daily JSONL file.
//
// Files are created with an explicit mode instead of relying on the process umask,
// because CPA usually runs as root and the files are meant to be readable by host-side
// tooling.
type jsonlWriter struct {
	mu          sync.Mutex
	dir         string
	file        *os.File
	buffer      *bufio.Writer
	currentDate string
	lastCleanup time.Time
}

func newJSONLWriter(dir string) *jsonlWriter {
	return &jsonlWriter{dir: dir}
}

func (w *jsonlWriter) write(line []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if errPrepare := w.prepareLocked(); errPrepare != nil {
		return errPrepare
	}
	if _, errWrite := w.buffer.Write(line); errWrite != nil {
		return errWrite
	}
	if errFlush := w.buffer.WriteByte('\n'); errFlush != nil {
		return errFlush
	}
	return nil
}

func (w *jsonlWriter) prepareLocked() error {
	today := time.Now().Format("2006-01-02")
	if w.file != nil && w.currentDate == today {
		return nil
	}
	if w.file != nil {
		_ = w.buffer.Flush()
		_ = w.file.Close()
		w.file = nil
		w.buffer = nil
	}
	if w.dir == "" {
		return fmt.Errorf("jsonl directory is empty")
	}
	if errMkdir := os.MkdirAll(w.dir, jsonlDirMode); errMkdir != nil {
		return errMkdir
	}
	path := filepath.Join(w.dir, jsonlFilePrefix+today+jsonlFileSuffix)
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, jsonlFileMode)
	if errOpen != nil {
		return errOpen
	}
	// An explicitly requested mode can still be narrowed by the umask, so correct it.
	_ = file.Chmod(jsonlFileMode)
	w.file = file
	w.buffer = bufio.NewWriterSize(file, 64*1024)
	w.currentDate = today
	if w.lastCleanup.IsZero() || time.Since(w.lastCleanup) > cleanupInterval {
		w.lastCleanup = time.Now()
		go w.cleanup()
	}
	return nil
}

// flush writes buffered lines to disk. It is called on shutdown and by the flusher.
func (w *jsonlWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buffer != nil {
		_ = w.buffer.Flush()
	}
}

func (w *jsonlWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buffer != nil {
		_ = w.buffer.Flush()
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = nil
	w.buffer = nil
}

// cleanup deletes JSONL files older than the retention window.
func (w *jsonlWriter) cleanup() {
	retention := currentConfig().RetentionDays
	if retention <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -retention)
	entries, errRead := os.ReadDir(w.dir)
	if errRead != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, jsonlFilePrefix) || !strings.HasSuffix(name, jsonlFileSuffix) {
			continue
		}
		datePart := strings.TrimSuffix(strings.TrimPrefix(name, jsonlFilePrefix), jsonlFileSuffix)
		fileDate, errParse := time.ParseInLocation("2006-01-02", datePart, time.Local)
		if errParse != nil {
			continue
		}
		if fileDate.Before(cutoff) {
			_ = os.Remove(filepath.Join(w.dir, name))
		}
	}
}

// sink owns the single JSONL writer and the shutdown barrier for queued writes.
type sink struct {
	once    sync.Once
	writer  *jsonlWriter
	queue   chan []byte
	done    chan struct{}
	stopped bool
	mu      sync.Mutex
}

var globalSink sink

// sinkWrite serializes one event and hands it to the writer without ever blocking a
// request path: a full queue drops the line and counts a write error.
func sinkWrite(cfg config, e *event) {
	if !cfg.JSONLEnabled || e == nil {
		return
	}
	encoded, errMarshal := json.Marshal(e)
	if errMarshal != nil {
		if st := currentStore(); st != nil {
			st.incWriteError()
		}
		return
	}
	globalSink.enqueue(cfg.JSONLDir, encoded)
}

func (s *sink) enqueue(dir string, line []byte) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.once.Do(func() { s.start(dir) })
	select {
	case s.queue <- line:
	default:
		if st := currentStore(); st != nil {
			st.incWriteError()
		}
	}
}

func (s *sink) start(dir string) {
	s.queue = make(chan []byte, sinkQueueSize)
	s.done = make(chan struct{})
	s.writer = newJSONLWriter(dir)
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(sinkFlushEvery)
		defer ticker.Stop()
		for {
			select {
			case line, ok := <-s.queue:
				if !ok {
					return
				}
				if errWrite := s.writer.write(line); errWrite != nil {
					if st := currentStore(); st != nil {
						st.incWriteError()
					}
				}
			case <-ticker.C:
				s.writer.flush()
			}
		}
	}()
}

// sinkShutdown flushes and stops the writer, waiting briefly for queued lines.
func sinkShutdown() {
	globalSink.mu.Lock()
	if globalSink.stopped || globalSink.queue == nil {
		globalSink.stopped = true
		globalSink.mu.Unlock()
		return
	}
	globalSink.stopped = true
	queue := globalSink.queue
	globalSink.mu.Unlock()
	close(queue)
	select {
	case <-globalSink.done:
	case <-time.After(3 * time.Second):
	}
	globalSink.writer.close()
}
