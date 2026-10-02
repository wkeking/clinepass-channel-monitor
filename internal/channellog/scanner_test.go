package channellog

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// deployLog writes one request log into a scanner directory the way CPA would have left it:
// with a name that ends in .log and an mtime old enough for the age gate.
func deployLog(t *testing.T, dir, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if errWrite := os.WriteFile(path, body, 0o644); errWrite != nil {
		t.Fatalf("write %s: %v", path, errWrite)
	}
	old := time.Now().Add(-time.Hour)
	if errChtimes := os.Chtimes(path, old, old); errChtimes != nil {
		t.Fatalf("backdate %s: %v", path, errChtimes)
	}
	return path
}

// newScanner builds a scanner over a fresh directory pair for one test.
func newScanner(t *testing.T, options Options) (*Scanner, string, string) {
	t.Helper()
	dir := t.TempDir()
	storeDir := t.TempDir()
	options.Enabled = true
	options.Dir = dir
	options.StoreDir = storeDir
	return New(options), dir, storeDir
}

// scannerHealth decodes the health snapshot the way a consumer sees it.
func scannerHealth(t *testing.T, scanner *Scanner) map[string]json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(scanner.Health())
	if errMarshal != nil {
		t.Fatalf("marshal health: %v", errMarshal)
	}
	var decoded map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("health must be a JSON object: %v", errUnmarshal)
	}
	return decoded
}

// TestScannerTurnsFixturesIntoStoredFacts drives a whole pass over the two frozen captures:
// both files become facts, both carry a channel, both are written to the day file and both are
// unlinked, because that is what the defaults ask for.
func TestScannerTurnsFixturesIntoStoredFacts(t *testing.T) {
	scanner, dir, storeDir := newScanner(t, Options{MinAge: 0, DeleteAfterRead: true})
	plainPath := deployLog(t, dir, "req-1.log", fixtureLog(t, fixturePlain))
	retryPath := deployLog(t, dir, "req-2.log", fixtureLog(t, fixtureRetry))

	stored, errScan := scanner.ScanOnce()
	if errScan != nil {
		t.Fatalf("ScanOnce: %v", errScan)
	}
	if stored != 2 {
		t.Fatalf("stored = %d, want 2", stored)
	}
	health := scanner.Health()
	if health.Scanned != 2 || health.Parsed != 2 || health.WithChannel != 2 || health.WithoutChannel != 0 {
		t.Errorf("health = %+v, want 2 scanned, 2 parsed, 2 with a channel", health)
	}
	if health.ParseFailures != 0 || health.SkippedYoung != 0 || health.SkippedGrowing != 0 || health.SkipMainLog != 0 {
		t.Errorf("health = %+v, want no failures and no skips", health)
	}
	if health.Deleted != 2 || health.DeletedBytes == 0 {
		t.Errorf("health = %+v, want both files unlinked with their bytes counted", health)
	}
	if health.PendingFiles != 0 {
		t.Errorf("pending_files = %d, want 0", health.PendingFiles)
	}
	if health.LastFactAt == nil {
		t.Error("last_fact_at must be set once a fact was stored")
	}
	if health.LastError != "" || health.LastErrorAt != nil {
		t.Errorf("last_error = %q at %v, want none", health.LastError, health.LastErrorAt)
	}
	for _, path := range []string{plainPath, retryPath} {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Errorf("%s must be unlinked after its fact was stored", path)
		}
	}

	// The facts land in one JSONL file named after the UTC day of the request, one fact per
	// line, and they are served newest first.
	day := filepath.Join(storeDir, "channel-log-2026-10-02.jsonl")
	raw, errRead := os.ReadFile(day)
	if errRead != nil {
		t.Fatalf("read the fact file: %v", errRead)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the fact file holds %d lines, want 2: %q", len(lines), raw)
	}
	for _, line := range lines {
		var decoded map[string]any
		if errUnmarshal := json.Unmarshal([]byte(line), &decoded); errUnmarshal != nil {
			t.Fatalf("a stored fact must be one JSON object per line: %v (%s)", errUnmarshal, line)
		}
	}
	facts := scanner.Facts()
	if len(facts) != 2 {
		t.Fatalf("facts = %d, want 2", len(facts))
	}
	if facts[0].SourceFile != "req-2.log" {
		t.Errorf("facts[0] came from %q, want the newest request first", facts[0].SourceFile)
	}
	if !facts[0].HadErrorResponse || facts[1].HadErrorResponse {
		t.Errorf("the retry fact must be the one flagged: %+v", facts)
	}
}

// TestYoungFileIsSkipped pins the age gate: a file CPA may still be appending to is left alone
// this round, and counted as skipped rather than as a failure.
func TestYoungFileIsSkipped(t *testing.T) {
	scanner, dir, _ := newScanner(t, Options{MinAge: time.Minute, DeleteAfterRead: true})
	path := filepath.Join(dir, "fresh.log")
	if errWrite := os.WriteFile(path, fixtureLog(t, fixturePlain), 0o644); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}

	stored, errScan := scanner.ScanOnce()
	if errScan != nil {
		t.Fatalf("ScanOnce: %v", errScan)
	}
	if stored != 0 {
		t.Errorf("stored = %d, want 0: the file is younger than min_age", stored)
	}
	health := scanner.Health()
	if health.SkippedYoung != 1 || health.Scanned != 0 || health.Deleted != 0 {
		t.Errorf("health = %+v, want exactly one skipped_young and nothing read or deleted", health)
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Errorf("a skipped file must stay in place: %v", errStat)
	}
}

// TestMainLogAndSubdirectoriesAreNeverRead pins the three exclusions in one directory: CPA's
// own main.log, the plugin's own store subdirectory, and a symlink that points outside. The
// files that must not be read are made unreadable, so an accidental open shows up as a parse
// failure rather than as a silent pass.
func TestMainLogAndSubdirectoriesAreNeverRead(t *testing.T) {
	scanner, dir, _ := newScanner(t, Options{MinAge: 0, DeleteAfterRead: true})
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not stop root, so the 'never opened' proof is not available")
	}
	deployLog(t, dir, "read-me.log", fixtureLog(t, fixturePlain))

	mainLog := filepath.Join(dir, mainLogName)
	if errWrite := os.WriteFile(mainLog, fixtureLog(t, fixtureRetry), 0o000); errWrite != nil {
		t.Fatalf("write main.log: %v", errWrite)
	}
	storePath := filepath.Join(dir, "channel-observation")
	if errMkdir := os.Mkdir(storePath, 0o755); errMkdir != nil {
		t.Fatalf("mkdir: %v", errMkdir)
	}
	own := filepath.Join(storePath, "channel-2026-10-02.jsonl")
	if errWrite := os.WriteFile(own, fixtureLog(t, fixtureRetry), 0o000); errWrite != nil {
		t.Fatalf("write the plugin's own store file: %v", errWrite)
	}
	nested := filepath.Join(storePath, "nested")
	if errMkdir := os.Mkdir(nested, 0o755); errMkdir != nil {
		t.Fatalf("mkdir nested: %v", errMkdir)
	}
	if errWrite := os.WriteFile(filepath.Join(nested, "deep.log"), fixtureLog(t, fixtureRetry), 0o000); errWrite != nil {
		t.Fatalf("write the nested log: %v", errWrite)
	}
	outside := filepath.Join(t.TempDir(), "target.log")
	if errWrite := os.WriteFile(outside, fixtureLog(t, fixtureRetry), 0o644); errWrite != nil {
		t.Fatalf("write the symlink target: %v", errWrite)
	}
	link := filepath.Join(dir, "link.log")
	if errSymlink := os.Symlink(outside, link); errSymlink != nil {
		t.Fatalf("symlink: %v", errSymlink)
	}

	stored, errScan := scanner.ScanOnce()
	if errScan != nil {
		t.Fatalf("ScanOnce: %v", errScan)
	}
	if stored != 1 {
		t.Fatalf("stored = %d, want only the one real request log", stored)
	}
	health := scanner.Health()
	if health.ParseFailures != 0 {
		t.Errorf("parse_failures = %d, want 0: no excluded file may have been opened", health.ParseFailures)
	}
	if health.SkipMainLog != 1 {
		t.Errorf("skip_main_log = %d, want 1", health.SkipMainLog)
	}
	if health.Scanned != 1 {
		t.Errorf("scanned = %d, want only read-me.log", health.Scanned)
	}
	facts := scanner.Facts()
	if len(facts) != 1 || facts[0].SourceFile != "read-me.log" {
		t.Errorf("facts = %+v, want the one readable request log", facts)
	}
	for _, path := range []string{mainLog, own, filepath.Join(nested, "deep.log"), outside, link} {
		if _, errStat := os.Stat(path); errStat != nil {
			// os.Stat follows the link, which is exactly what the scanner must not do.
			t.Errorf("%s must still exist: %v", path, errStat)
		}
	}
}

// TestDeletionIsConfigurableAndOnlyForStoredFacts covers both deletion switches and the one
// case that must never delete: a file the scanner could not read.
func TestDeletionIsConfigurableAndOnlyForStoredFacts(t *testing.T) {
	unreadable := os.Geteuid() != 0

	t.Run("delete_after_read", func(t *testing.T) {
		scanner, dir, _ := newScanner(t, Options{MinAge: 0, DeleteAfterRead: true})
		good := deployLog(t, dir, "good.log", fixtureLog(t, fixturePlain))
		bad := deployLog(t, dir, "unreadable.log", fixtureLog(t, fixtureRetry))
		if unreadable {
			if errChmod := os.Chmod(bad, 0o000); errChmod != nil {
				t.Fatalf("chmod: %v", errChmod)
			}
		}
		stored, errScan := scanner.ScanOnce()
		if errScan == nil && unreadable {
			// The unreadable file is reported; the pass still stores the readable one.
			t.Errorf("ScanOnce must report the first failure of the pass")
		}
		if stored != 1 {
			t.Fatalf("stored = %d, want 1", stored)
		}
		if _, errStat := os.Stat(good); !os.IsNotExist(errStat) {
			t.Error("a parsed file must be unlinked when delete_after_read is on")
		}
		if unreadable {
			if _, errStat := os.Stat(bad); errStat != nil {
				t.Errorf("a file that was not parsed must stay: %v", errStat)
			}
			if health := scanner.Health(); health.ParseFailures != 1 || health.LastError == "" {
				t.Errorf("health = %+v, want one parse failure with a reason", health)
			}
			if health := scanner.Health(); health.Deleted != 1 {
				t.Errorf("deleted = %d, want only the parsed file", health.Deleted)
			}
		}
	})

	t.Run("no_delete", func(t *testing.T) {
		scanner, dir, _ := newScanner(t, Options{MinAge: 0, DeleteAfterRead: false})
		path := deployLog(t, dir, "keep.log", fixtureLog(t, fixturePlain))
		stored, errScan := scanner.ScanOnce()
		if errScan != nil {
			t.Fatalf("ScanOnce: %v", errScan)
		}
		if stored != 1 {
			t.Fatalf("stored = %d, want 1", stored)
		}
		if _, errStat := os.Stat(path); errStat != nil {
			t.Errorf("delete_after_read is off, so the log must stay: %v", errStat)
		}
		if health := scanner.Health(); health.Deleted != 0 || health.DeletedBytes != 0 {
			t.Errorf("health = %+v, want nothing deleted", health)
		}
	})
}

// TestFileThatGrewIsSkipped covers both halves of the growth gate: a file that changed between
// the directory listing and the size check, and one that changed between the size check and the
// read. Neither is stored and neither is deleted.
func TestFileThatGrewIsSkipped(t *testing.T) {
	t.Run("between listing and size check", func(t *testing.T) {
		scanner, dir, _ := newScanner(t, Options{MinAge: 0, DeleteAfterRead: true})
		path := deployLog(t, dir, "growing.log", fixtureLog(t, fixturePlain))
		scanner.testAfterInfo = func(name string) {
			appendToFile(t, path, "\n=== API RESPONSE 2 ===\n")
		}
		stored, errScan := scanner.ScanOnce()
		if errScan != nil {
			t.Fatalf("ScanOnce: %v", errScan)
		}
		if stored != 0 {
			t.Errorf("stored = %d, want 0", stored)
		}
		assertGrowing(t, scanner, path)
	})

	t.Run("between size check and read", func(t *testing.T) {
		scanner, dir, _ := newScanner(t, Options{MinAge: 0, DeleteAfterRead: true})
		path := deployLog(t, dir, "growing.log", fixtureLog(t, fixturePlain))
		scanner.testAfterStat = func(name string) {
			appendToFile(t, path, "\n=== API RESPONSE 2 ===\n")
		}
		stored, errScan := scanner.ScanOnce()
		if errScan != nil {
			t.Fatalf("ScanOnce: %v", errScan)
		}
		if stored != 0 {
			t.Errorf("stored = %d, want 0: the parse saw only part of the file", stored)
		}
		assertGrowing(t, scanner, path)
	})
}

func assertGrowing(t *testing.T, scanner *Scanner, path string) {
	t.Helper()
	health := scanner.Health()
	if health.SkippedGrowing != 1 {
		t.Errorf("skipped_growing = %d, want 1", health.SkippedGrowing)
	}
	if health.Parsed != 0 || health.Deleted != 0 {
		t.Errorf("health = %+v, want nothing parsed and nothing deleted", health)
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Errorf("a file that grew must stay for the next pass: %v", errStat)
	}
	if len(scanner.Facts()) != 0 {
		t.Errorf("facts = %+v, want none", scanner.Facts())
	}
}

func appendToFile(t *testing.T, path, text string) {
	t.Helper()
	handle, errOpen := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if errOpen != nil {
		t.Fatalf("open for append: %v", errOpen)
	}
	if _, errWrite := handle.WriteString(text); errWrite != nil {
		_ = handle.Close()
		t.Fatalf("append: %v", errWrite)
	}
	if errClose := handle.Close(); errClose != nil {
		t.Fatalf("close: %v", errClose)
	}
}

// TestUnstorableStoreIsFailOpen is the disk-full case: the parse succeeded but the fact could
// not be written, so the file is kept, the failure is counted, and nothing has been deleted.
func TestUnstorableStoreIsFailOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not stop root")
	}
	dir := t.TempDir()
	path := deployLog(t, dir, "good.log", fixtureLog(t, fixturePlain))

	readOnly := t.TempDir()
	if errChmod := os.Chmod(readOnly, 0o555); errChmod != nil {
		t.Fatalf("chmod: %v", errChmod)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
	// The scanner is built with the unwritable store directory rather than having it patched
	// in afterwards: the writer takes its directory from Options.
	scanner := New(Options{
		Enabled:         true,
		Dir:             dir,
		StoreDir:        filepath.Join(readOnly, "facts"),
		MinAge:          0,
		DeleteAfterRead: true,
	})

	stored, errScan := scanner.ScanOnce()
	if errScan == nil {
		t.Error("ScanOnce must report the store failure")
	}
	if stored != 0 {
		t.Errorf("stored = %d, want 0", stored)
	}
	health := scanner.Health()
	if health.ParseFailures != 1 || health.LastError == "" || health.LastErrorAt == nil {
		t.Errorf("health = %+v, want one counted failure with a reason and a time", health)
	}
	if health.Deleted != 0 {
		t.Errorf("deleted = %d, want 0: a fact that did not persist may not unlink its source", health.Deleted)
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Errorf("the source file must stay: %v", errStat)
	}
}

// TestDisabledScannerIsInert pins the switch: with Enabled false there is no goroutine, no
// directory access at all, and no error to report — not even for a directory that is not there.
func TestDisabledScannerIsInert(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	scanner := New(Options{Enabled: false, Dir: dir, StoreDir: t.TempDir()})
	scanner.Start()
	scanner.Start()
	scanner.Stop()
	scanner.Stop()

	stored, errScan := scanner.ScanOnce()
	if errScan != nil || stored != 0 {
		t.Errorf("ScanOnce on a disabled scanner = (%d, %v), want (0, nil)", stored, errScan)
	}
	health := scanner.Health()
	if health.Enabled || health.Scanned != 0 || health.LastError != "" {
		t.Errorf("health = %+v, want a disabled scanner that never touched the directory", health)
	}
	if _, errStat := os.Stat(dir); !os.IsNotExist(errStat) {
		t.Errorf("the scanner must not create or read the directory: %v", errStat)
	}

	// The same over a directory that does exist and holds a log: still nothing happens.
	present := t.TempDir()
	path := deployLog(t, present, "untouched.log", fixtureLog(t, fixturePlain))
	scanner = New(Options{Enabled: false, Dir: present, StoreDir: t.TempDir()})
	if stored, errScan := scanner.ScanOnce(); stored != 0 || errScan != nil {
		t.Errorf("ScanOnce on a disabled scanner = (%d, %v), want (0, nil)", stored, errScan)
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Errorf("a disabled scanner must leave the directory alone: %v", errStat)
	}
}

// TestStartPollsUntilStop pins the lifecycle the plugin drives: Start scans without waiting for
// the first tick, and Stop returns only once the pass in flight is over.
func TestStartPollsUntilStop(t *testing.T) {
	scanner, dir, _ := newScanner(t, Options{MinAge: 0, DeleteAfterRead: true, PollInterval: 10 * time.Millisecond})
	deployLog(t, dir, "req.log", fixtureLog(t, fixturePlain))
	scanner.Start()
	scanner.Start()

	deadline := time.Now().Add(3 * time.Second)
	for scanner.Health().Parsed == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := scanner.Health().Parsed; got != 1 {
		t.Fatalf("parsed = %d, want the first pass to run without waiting for a tick", got)
	}
	scanner.Stop()
	scanner.Stop()
	if scanner.Health().Enabled != true {
		t.Error("health must keep reporting the scanner's configured state after Stop")
	}
}

// TestStopRacesTheFirstPass pins the case every plugin reload hits: Stop lands while the loop
// is still inside its first pass, possibly before it ever reached a tick. The loop must watch
// the channel it was started with, or Stop waits on a goroutine that can never come back.
func TestStopRacesTheFirstPass(t *testing.T) {
	for attempt := 0; attempt < 25; attempt++ {
		scanner, dir, _ := newScanner(t, Options{MinAge: 0, PollInterval: time.Millisecond})
		deployLog(t, dir, "req.log", fixtureLog(t, fixturePlain))
		scanner.Start()
		stopped := make(chan struct{})
		go func() {
			scanner.Stop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatalf("Stop did not return on attempt %d: the loop must watch the channel it was given", attempt)
		}
	}
}

// TestMemoryIsBoundedByTheBufferNotTheFile is the acceptance test for a 6 MB log. The body of a
// long context is one enormous line; the parser must stream past it. Two independent witnesses
// are asserted: no single read of the underlying file is larger than the streaming buffer, and
// the whole parse allocates far less than the file's own size. A design that read the file into
// memory would fail both — io.ReadAll asks for multi-megabyte reads, and os.ReadFile allocates
// the file.
func TestMemoryIsBoundedByTheBufferNotTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	file, errCreate := os.Create(path)
	if errCreate != nil {
		t.Fatalf("create: %v", errCreate)
	}
	// One 6 MB line, written from a reused block so the test itself does not hold it.
	block := []byte(strings.Repeat("x", 1<<20))
	if _, errWrite := file.WriteString("=== REQUEST INFO ===\nVersion: v8.0.8\nURL: /v1/responses\nMethod: POST\n" +
		"Timestamp: 2026-10-02T22:30:00.000000000+08:00\n\n=== REQUEST BODY ===\n"); errWrite != nil {
		t.Fatalf("write header: %v", errWrite)
	}
	for write := 0; write < 6; write++ {
		if _, errWrite := file.Write(block); errWrite != nil {
			t.Fatalf("write body: %v", errWrite)
		}
	}
	if _, errWrite := file.WriteString("\n\n=== API RESPONSE 1 ===\n" +
		channelFrame("deepseek", "deepseek", "deepseek/deepseek-v4.1-flash", 1) + "\ndata: [DONE]\n"); errWrite != nil {
		t.Fatalf("write response: %v", errWrite)
	}
	block = nil
	if errClose := file.Close(); errClose != nil {
		t.Fatalf("close: %v", errClose)
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		t.Fatalf("stat: %v", errStat)
	}
	if info.Size() < 6<<20 {
		t.Fatalf("the fixture is %d bytes, want at least 6 MB", info.Size())
	}

	handle, errOpen := os.Open(path)
	if errOpen != nil {
		t.Fatalf("open: %v", errOpen)
	}
	defer func() { _ = handle.Close() }()
	tracked := &trackedReader{reader: handle}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fact, errParse := parseLog(tracked, "big.log", time.Now())
	runtime.ReadMemStats(&after)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if fact.FinalProvider != "deepseek" || fact.Frames != 1 {
		t.Errorf("the 6 MB log must still parse: %+v", fact)
	}
	if tracked.bytes != info.Size() {
		t.Errorf("read %d bytes of a %d byte file, want the whole file streamed", tracked.bytes, info.Size())
	}
	if tracked.maxRequest > readBuffer {
		t.Errorf("the largest single read was %d bytes, want at most the %d byte buffer", tracked.maxRequest, readBuffer)
	}
	allocated := int64(after.TotalAlloc - before.TotalAlloc)
	if allocated > info.Size()/2 {
		t.Errorf("parsing a %d byte log allocated %d bytes, want far less than the file: it must be streamed",
			info.Size(), allocated)
	}
	t.Logf("a %d byte log: largest read request %d bytes, %d bytes streamed, %d bytes allocated",
		info.Size(), tracked.maxRequest, tracked.bytes, allocated)
}

// trackedReader records the largest read request and the byte count of everything it handed
// out, which is how "streamed" is told apart from "loaded".
type trackedReader struct {
	reader     io.Reader
	maxRequest int
	bytes      int64
}

func (r *trackedReader) Read(buffer []byte) (int, error) {
	if len(buffer) > r.maxRequest {
		r.maxRequest = len(buffer)
	}
	read, errRead := r.reader.Read(buffer)
	r.bytes += int64(read)
	return read, errRead
}

// TestScannerParsesTheSixMegabyteLog is the same file through the scanner, so the acceptance
// case is covered end to end rather than only at the parser.
func TestScannerParsesTheSixMegabyteLog(t *testing.T) {
	scanner, dir, _ := newScanner(t, Options{MinAge: 0, DeleteAfterRead: true})
	path := filepath.Join(dir, "big.log")
	file, errCreate := os.Create(path)
	if errCreate != nil {
		t.Fatalf("create: %v", errCreate)
	}
	block := []byte(strings.Repeat("y", 1<<20))
	if _, errWrite := file.WriteString("=== REQUEST INFO ===\nURL: /v1/responses\nMethod: POST\n" +
		"Timestamp: 2026-10-02T22:31:00.000000000+08:00\n\n=== REQUEST BODY ===\n"); errWrite != nil {
		t.Fatalf("write: %v", errWrite)
	}
	for write := 0; write < 6; write++ {
		if _, errWrite := file.Write(block); errWrite != nil {
			t.Fatalf("write body: %v", errWrite)
		}
	}
	if _, errWrite := file.WriteString("\n\n=== API RESPONSE 1 ===\n" +
		channelFrame("deepseek", "deepseek", "deepseek/deepseek-v4.1-flash", 1) + "\n"); errWrite != nil {
		t.Fatalf("write response: %v", errWrite)
	}
	if errClose := file.Close(); errClose != nil {
		t.Fatalf("close: %v", errClose)
	}
	old := time.Now().Add(-time.Hour)
	if errChtimes := os.Chtimes(path, old, old); errChtimes != nil {
		t.Fatalf("backdate: %v", errChtimes)
	}

	stored, errScan := scanner.ScanOnce()
	if errScan != nil {
		t.Fatalf("ScanOnce: %v", errScan)
	}
	if stored != 1 {
		t.Fatalf("stored = %d, want 1", stored)
	}
	facts := scanner.Facts()
	if len(facts) != 1 || facts[0].FinalProvider != "deepseek" {
		t.Fatalf("facts = %+v, want the channel from the section after the long body", facts)
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Error("the parsed 6 MB log must still be unlinked")
	}
}

// TestFactsAreCappedAndNewestFirst pins the ring the management API reads from.
func TestFactsAreCappedAndNewestFirst(t *testing.T) {
	scanner, dir, _ := newScanner(t, Options{MinAge: 0, DeleteAfterRead: false, MaxFacts: 1})
	deployLog(t, dir, "req-1.log", fixtureLog(t, fixturePlain))
	deployLog(t, dir, "req-2.log", fixtureLog(t, fixtureRetry))

	if _, errScan := scanner.ScanOnce(); errScan != nil {
		t.Fatalf("ScanOnce: %v", errScan)
	}
	facts := scanner.Facts()
	if len(facts) != 1 {
		t.Fatalf("facts = %d, want the cap of 1", len(facts))
	}
	if facts[0].SourceFile != "req-2.log" {
		t.Errorf("the kept fact came from %q, want the newest request", facts[0].SourceFile)
	}
	if scanner.Health().Parsed != 2 {
		t.Errorf("parsed = %d, want both facts counted even though only one is held in memory",
			scanner.Health().Parsed)
	}
}

// TestHealthCarriesEveryKey pins the health contract: the keys are exact, and they are all
// present on a scanner that has never run, so a page or an operator can read the disabled
// state without guessing.
func TestHealthCarriesEveryKey(t *testing.T) {
	scanner := New(Options{Enabled: false, Dir: DefaultDir})
	decoded := scannerHealth(t, scanner)
	want := []string{
		"enabled", "directory", "scanned", "parsed", "with_channel", "without_channel",
		"parse_failures", "deleted", "deleted_bytes", "skipped_young", "skipped_growing",
		"skipped_seen", "skip_main_log", "pruned", "pruned_bytes", "last_fact_at",
		"last_error", "last_error_at", "pending_files",
	}
	for _, key := range want {
		if _, ok := decoded[key]; !ok {
			t.Errorf("health is missing the %q key", key)
		}
	}
	if len(decoded) != len(want) {
		t.Errorf("health carries %d keys, want exactly %d: %v", len(decoded), len(want), decoded)
	}
}

// TestSecondPassWithDeletionOffDoesNotStoreDuplicates pins the seen-set. A deployment that
// keeps CPA's log files (delete_after_read=false) must still store each request exactly once:
// without this the scanner re-parses every file on every pass and the fact store counts one
// request many times, which corrupts every ratio computed from it.
func TestSecondPassWithDeletionOffDoesNotStoreDuplicates(t *testing.T) {
	scanner, dir, storeDir := newScanner(t, Options{MinAge: 0, DeleteAfterRead: false})
	path := deployLog(t, dir, "req-1.log", fixtureLog(t, fixturePlain))

	if stored, errScan := scanner.ScanOnce(); errScan != nil || stored != 1 {
		t.Fatalf("first pass: stored = %d, err = %v, want 1 fact", stored, errScan)
	}
	if stored, errScan := scanner.ScanOnce(); errScan != nil || stored != 0 {
		t.Fatalf("second pass: stored = %d, err = %v, want the file to be recognised as done", stored, errScan)
	}
	health := scanner.Health()
	if health.SkippedSeen != 1 {
		t.Errorf("skipped_seen = %d, want 1", health.SkippedSeen)
	}
	if health.Scanned != 1 || health.Parsed != 1 || health.PendingFiles != 0 {
		t.Errorf("health = %+v, want the one file read exactly once and nothing left pending", health)
	}
	if _, errStat := os.Stat(path); errStat != nil {
		t.Errorf("delete_after_read=false must leave the file in place: %v", errStat)
	}
	raw, errRead := os.ReadFile(filepath.Join(storeDir, "channel-log-2026-10-02.jsonl"))
	if errRead != nil {
		t.Fatalf("read the fact file: %v", errRead)
	}
	if got := len(strings.Split(strings.TrimRight(string(raw), "\n"), "\n")); got != 1 {
		t.Errorf("the fact file holds %d lines, want 1: %q", got, raw)
	}
	// A file whose size changed is a different file (CPA never rewrites a finished log, but a
	// new capture can land under the same name): it must be parsed again.
	appendToFile(t, path, "\n")
	if stored, errScan := scanner.ScanOnce(); errScan != nil || stored != 1 {
		t.Fatalf("after the file changed: stored = %d, err = %v, want 1", stored, errScan)
	}
}

// TestExpiredFactFilesArePruned pins retention: the fact files carry the UTC date in their
// name, so an expired day is deleted while the current one is kept. Without this the sidecar
// would grow forever beside records that the observation store has already expired.
func TestExpiredFactFilesArePruned(t *testing.T) {
	scanner, dir, storeDir := newScanner(t, Options{MinAge: 0, RetentionDays: 3})
	old := filepath.Join(storeDir, "channel-log-2020-01-01.jsonl")
	current := filepath.Join(storeDir, "channel-log-2026-10-02.jsonl")
	for _, path := range []string{old, current} {
		if errWrite := os.WriteFile(path, []byte("{\"time\":\"2026-10-02T00:00:00Z\"}\n"), 0o644); errWrite != nil {
			t.Fatalf("seed %s: %v", path, errWrite)
		}
	}
	if _, errScan := scanner.ScanOnce(); errScan != nil {
		t.Fatalf("ScanOnce: %v", errScan)
	}
	if _, errStat := os.Stat(old); !os.IsNotExist(errStat) {
		t.Errorf("the expired day must be pruned, stat err = %v", errStat)
	}
	if _, errStat := os.Stat(current); errStat != nil {
		t.Errorf("the current day must be kept: %v", errStat)
	}
	if health := scanner.Health(); health.Pruned != 1 || health.PrunedBytes == 0 {
		t.Errorf("health = %+v, want exactly one pruned file with its bytes counted", health)
	}
	_ = dir
}

// TestFactCarriesEveryKey pins the fact contract, including the fields a failed request leaves
// empty: the merger reads keys, not struct fields, and a missing key is a different bug from an
// empty one.
func TestFactCarriesEveryKey(t *testing.T) {
	raw, errMarshal := json.Marshal(Fact{})
	if errMarshal != nil {
		t.Fatalf("marshal fact: %v", errMarshal)
	}
	var decoded map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("fact must be a JSON object: %v", errUnmarshal)
	}
	want := []string{
		"time", "path", "method", "session_id", "session_uuid", "has_session",
		"final_provider", "resolved_provider", "canonical_slug", "original_model_id",
		"pinned_provider", "affinity_outcome", "model_attempt_count",
		"total_provider_attempt_count", "fallbacks_available", "gateway_cost",
		"frames", "attempts_seen", "had_error_response", "source_file", "parsed_at",
	}
	for _, key := range want {
		if _, ok := decoded[key]; !ok {
			t.Errorf("fact is missing the %q key", key)
		}
	}
	if len(decoded) != len(want) {
		t.Errorf("fact carries %d keys, want exactly %d: %v", len(decoded), len(want), decoded)
	}
	// A parsed fact never carries a null list: only a hand-built zero Fact does.
	fact, errParse := parseLog(strings.NewReader(fixtureLogString(fixturePlain)), fixturePlain, time.Now())
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	encoded, errMarshal := json.Marshal(fact)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	if !strings.Contains(string(encoded), `"fallbacks_available":[]`) {
		t.Errorf("a parsed fact must carry an empty list, not null: %s", encoded)
	}
}

// fixtureLogString is fixtureLog for callers that only need the text.
func fixtureLogString(name string) string {
	raw, errRead := os.ReadFile(filepath.Join("testdata", name))
	if errRead != nil {
		panic(errRead)
	}
	return string(raw)
}

// TestMissingDirectoryOnlyMovesAHealthCounter pins fail-open: the failure is reported to the
// caller and to health, and it stays there — nothing panics, and nothing counts as parsed.
func TestMissingDirectoryOnlyMovesAHealthCounter(t *testing.T) {
	scanner := New(Options{Enabled: true, Dir: filepath.Join(t.TempDir(), "absent"), StoreDir: t.TempDir()})
	stored, errScan := scanner.ScanOnce()
	if errScan == nil {
		t.Error("ScanOnce must report the unreadable directory")
	}
	if stored != 0 {
		t.Errorf("stored = %d, want 0", stored)
	}
	health := scanner.Health()
	if health.LastError == "" || health.LastErrorAt == nil {
		t.Errorf("health = %+v, want the reason and its time", health)
	}
	if health.Parsed != 0 || health.WithChannel != 0 {
		t.Errorf("health = %+v, want nothing parsed", health)
	}
}
