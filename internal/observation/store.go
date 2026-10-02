package observation

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The store is append-only JSONL, one file per UTC day:
//
//	channel-2026-10-02.jsonl
//
// A file per day is what makes both ends of the retention policy cheap — deleting a day is
// deleting a file, and reading a window is reading at most a handful of them. Writes are
// buffered and flushed on a timer, and every failure is recorded in health instead of being
// reported to the stream: a broken disk must not break the proxy.
const (
	filePrefix   = "channel-"
	fileSuffix   = ".jsonl"
	dateLayout   = "2006-01-02"
	flushBytes   = 64 << 10
	scanLineCap  = 4 << 20
	directoryFor = 0o755
	fileFor      = 0o644
)

type store struct {
	mu sync.Mutex

	dir           string
	retentionDays int
	maxSizeMB     int

	file     *os.File
	writer   *bufio.Writer
	fileDate string
	pending  int

	health storeHealth
}

type storeHealth struct {
	Directory string
	Files     int
	Bytes     int64
	LastError string
}

// openStore prepares the directory. A directory that cannot be created is not fatal: the
// recorder keeps aggregating in memory and health carries the reason.
func openStore(options Options) *store {
	instance := &store{
		dir:           options.Directory,
		retentionDays: options.RetentionDays,
		maxSizeMB:     options.MaxSizeMB,
	}
	instance.health.Directory = options.Directory
	if errMkdir := os.MkdirAll(options.Directory, directoryFor); errMkdir != nil {
		instance.health.LastError = fmt.Sprintf("create store directory: %v", errMkdir)
		return instance
	}
	instance.refresh()
	return instance
}

// append writes one record. It rotates at the UTC day boundary and buffers until the flush
// threshold, so a burst of records costs one write.
func (s *store) append(record Record) error {
	if s == nil {
		return nil
	}
	encoded, errEncode := json.Marshal(record)
	if errEncode != nil {
		return fmt.Errorf("encode record: %w", errEncode)
	}
	encoded = append(encoded, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.health.LastError != "" && s.file == nil && !s.directoryUsable() {
		// Keep reporting the original reason instead of a new failure per record.
		return errors.New(s.health.LastError)
	}
	date := record.Time.UTC().Format(dateLayout)
	if s.file == nil || date != s.fileDate {
		if errRotate := s.rotateLocked(date); errRotate != nil {
			s.health.LastError = errRotate.Error()
			return errRotate
		}
	}
	if _, errWrite := s.writer.Write(encoded); errWrite != nil {
		s.health.LastError = fmt.Sprintf("append record: %v", errWrite)
		return errors.New(s.health.LastError)
	}
	s.pending += len(encoded)
	if s.pending >= flushBytes {
		if errFlush := s.writer.Flush(); errFlush != nil {
			s.health.LastError = fmt.Sprintf("flush records: %v", errFlush)
			return errors.New(s.health.LastError)
		}
		s.pending = 0
	}
	return nil
}

// flush pushes the buffer to the operating system.
func (s *store) flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil {
		return nil
	}
	if errFlush := s.writer.Flush(); errFlush != nil {
		s.health.LastError = fmt.Sprintf("flush records: %v", errFlush)
		return errFlush
	}
	s.pending = 0
	return nil
}

// close flushes and closes the open file.
func (s *store) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer != nil {
		_ = s.writer.Flush()
	}
	if s.file != nil {
		_ = s.file.Close()
	}
	s.writer = nil
	s.file = nil
}

// rotateLocked switches to the file of a new day.
func (s *store) rotateLocked(date string) error {
	if s.writer != nil {
		_ = s.writer.Flush()
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
		s.writer = nil
	}
	path := filepath.Join(s.dir, filePrefix+date+fileSuffix)
	handle, errOpen := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, fileFor)
	if errOpen != nil {
		return fmt.Errorf("open record file: %w", errOpen)
	}
	s.file = handle
	s.writer = bufio.NewWriterSize(handle, flushBytes)
	s.fileDate = date
	s.pending = 0
	if s.health.LastError != "" && strings.HasPrefix(s.health.LastError, "open record file") {
		s.health.LastError = ""
	}
	return nil
}

// directoryUsable reports whether the configured directory can be written to.
func (s *store) directoryUsable() bool {
	info, errStat := os.Stat(s.dir)
	if errStat != nil || !info.IsDir() {
		return false
	}
	return true
}

// refresh recomputes the file count and the total size.
func (s *store) refresh() {
	files := s.list()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health.Files = len(files)
	var total int64
	for _, entry := range files {
		total += entry.size
	}
	s.health.Bytes = total
}

// clean enforces the retention window and then the size cap, oldest file first.
func (s *store) clean(now time.Time) storeHealth {
	if s == nil {
		return storeHealth{}
	}
	files := s.list()
	cutoff := now.UTC().AddDate(0, 0, -s.retentionDays)
	kept := make([]storedFile, 0, len(files))
	for _, entry := range files {
		if entry.date.Before(cutoff) {
			if errRemove := os.Remove(entry.path); errRemove != nil {
				s.mu.Lock()
				s.health.LastError = fmt.Sprintf("remove old record file: %v", errRemove)
				s.mu.Unlock()
				kept = append(kept, entry)
			}
			continue
		}
		kept = append(kept, entry)
	}
	limit := int64(s.maxSizeMB) << 20
	var total int64
	for _, entry := range kept {
		total += entry.size
	}
	for index := 0; index < len(kept) && total > limit; index++ {
		entry := kept[index]
		if errRemove := os.Remove(entry.path); errRemove != nil {
			s.mu.Lock()
			s.health.LastError = fmt.Sprintf("remove oversized record file: %v", errRemove)
			s.mu.Unlock()
			continue
		}
		total -= entry.size
		kept = append(kept[:index], kept[index+1:]...)
		index--
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health.Files = len(kept)
	s.health.Bytes = total
	return s.health
}

func (s *store) healthSnapshot() storeHealth {
	if s == nil {
		return storeHealth{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health
}

type storedFile struct {
	path string
	date time.Time
	size int64
}

// list returns the record files of this store, oldest first. Files that do not match the
// naming scheme are ignored, which is what keeps a foreign file in the directory safe.
func (s *store) list() []storedFile {
	if s == nil || s.dir == "" {
		return nil
	}
	entries, errRead := os.ReadDir(s.dir)
	if errRead != nil {
		s.mu.Lock()
		s.health.LastError = fmt.Sprintf("read store directory: %v", errRead)
		s.mu.Unlock()
		return nil
	}
	out := make([]storedFile, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		date, errParse := time.ParseInLocation(dateLayout, strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix), time.UTC)
		if errParse != nil {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		out = append(out, storedFile{path: filepath.Join(s.dir, name), date: date, size: info.Size()})
	}
	sort.Slice(out, func(first, second int) bool {
		return out[first].date.Before(out[second].date)
	})
	return out
}

// scanLimits bounds a read so that querying a week cannot pull the whole directory into
// memory at once.
type scanLimits struct {
	MaxBytes int64
}

type scanStats struct {
	Records   int
	Bytes     int64
	Truncated bool
}

// scan reads the stored records at or after from, oldest first. Byte limits and decode
// errors stop the read instead of failing the query: a partial view is more useful than an
// error page, and the caller reports how much it read.
func (s *store) scan(from time.Time, limits scanLimits, fn func(Record) error) (scanStats, error) {
	stats := scanStats{}
	if s == nil {
		return stats, nil
	}
	if s.writer != nil {
		// Records still in the buffer are part of the history a query must see.
		_ = s.flush()
	}
	for _, entry := range s.list() {
		if entry.date.AddDate(0, 0, 1).Before(from) {
			continue
		}
		file, errOpen := os.Open(entry.path)
		if errOpen != nil {
			return stats, fmt.Errorf("open record file: %w", errOpen)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64<<10), scanLineCap)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var record Record
			if errUnmarshal := json.Unmarshal(line, &record); errUnmarshal != nil {
				continue
			}
			if record.Time.Before(from) {
				continue
			}
			if limits.MaxBytes > 0 && stats.Bytes >= limits.MaxBytes {
				stats.Truncated = true
				_ = file.Close()
				return stats, nil
			}
			if errCall := fn(record); errCall != nil {
				_ = file.Close()
				return stats, errCall
			}
			stats.Records++
			stats.Bytes += int64(len(line))
		}
		_ = file.Close()
	}
	return stats, nil
}

// loadSince streams every record at or after from into fn.
func (s *store) loadSince(from time.Time, fn func(Record)) (int, bool, error) {
	stats, errScan := s.scan(from, scanLimits{}, func(record Record) error {
		fn(record)
		return nil
	})
	return stats.Records, stats.Truncated, errScan
}

// loadRecent is loadSince with a byte budget, used to rebuild the aggregates at startup.
func (s *store) loadRecent(window time.Duration, maxBytes int64, fn func(Record)) (int, bool, error) {
	stats, errScan := s.scan(time.Now().UTC().Add(-window), scanLimits{MaxBytes: maxBytes}, func(record Record) error {
		fn(record)
		return nil
	})
	return stats.Records, stats.Truncated, errScan
}

// writeCSV streams the stored records of a window as CSV. The columns are the ones the
// operator question is answered with, plus the identity of the request so a row can be
// traced back to the raw chunk it came from.
func (r *Recorder) WriteCSV(window Window, sink io.Writer) (int, error) {
	if r == nil || r.store == nil {
		return 0, nil
	}
	writer := csv.NewWriter(bufio.NewWriterSize(sink, 64<<10))
	if errHeader := writer.Write(csvHeader()); errHeader != nil {
		return 0, errHeader
	}
	written := 0
	since := r.now().Add(-window.Duration())
	_, errScan := r.store.scan(since, scanLimits{}, func(record Record) error {
		if errWrite := writer.Write(csvRecord(record, r.options.Baseline)); errWrite != nil {
			return errWrite
		}
		written++
		return nil
	})
	if errScan != nil {
		return written, errScan
	}
	writer.Flush()
	return written, writer.Error()
}
