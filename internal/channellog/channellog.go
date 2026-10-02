// Package channellog reads what CPA's per-request debug log knows and the usage hook does not:
// which upstream channel the gateway actually answered on.
//
// CPA writes one log file per request while `observability.logs.request-log` is on and
// `server.commercial-mode` is off. For a client speaking POST /v1/responses those files hold
// the upstream chat/completions response verbatim, and that response carries
// choices[].delta.provider_metadata.gateway.routing — the block the Responses translation
// drops before the client ever sees it. The log is therefore the only per-request source of
// finalProvider/resolvedProvider on this deployment.
//
// The scanner runs beside the request path, never on it:
//
//   - it polls a directory, and everything it does is fail-open. A missing directory, an
//     unreadable file, a frame that does not decode and a failed unlink each move one health
//     counter and nothing else. This code can never fail a proxied request.
//   - the files are read with a fixed buffer, so a 6 MB log costs the buffer rather than the
//     file, and the plaintext prompt inside a request body is never retained.
//   - only the top level of the directory is listed, `.log` files only, and `main.log` is
//     excluded. The plugin's own JSONL store lives in a SUBDIRECTORY of the same directory,
//     which is one of the reasons the scan never recurses.
//
// The fact it produces carries the join key the later merge needs: the log's Timestamp (the
// request arrival) and the Session_id header, whose uuid also reaches the observation store
// inside "codex:session-<uuid>".
package channellog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultDir is the directory CPA writes its request logs into. On the host it is
	// /opt/cpa/logs; inside the container it is this path.
	DefaultDir = "/CLIProxyAPI/logs"
	// DefaultMinAge is how long a file must have been untouched before it is read. It is what
	// keeps a file CPA is still appending to from being parsed half-way.
	DefaultMinAge = 5 * time.Second
	// DefaultPollInterval is how often the directory is listed.
	DefaultPollInterval = 2 * time.Second
	// DefaultMaxFacts bounds the in-memory list the management API serves. The facts are on
	// disk as well; the ring only exists so the page can render them without reading files.
	DefaultMaxFacts = 200
	// DefaultStoreDir is where the facts are appended when the caller names no directory. It
	// is the observation store's own directory, which is a subdirectory of DefaultDir: the
	// facts belong beside the usage records they are joined with.
	DefaultStoreDir = "/CLIProxyAPI/logs/channel-observation"
	// DefaultRetentionDays matches the plugin's channel_retention_days default: the fact files
	// live beside the observation records and are pruned on the same clock.
	DefaultRetentionDays = 3
)

// Options is the scanner's configuration. The zero value is not useful: the directory and the
// store directory fall back to their defaults, but Enabled must be set.
type Options struct {
	Enabled bool
	// Dir is the directory CPA writes its request logs into. It is never recursed into.
	Dir string
	// StoreDir is the plugin's channel store directory: the facts are appended to
	// channel-log-<UTC date>.jsonl inside it.
	StoreDir string
	// MinAge is how old a file must be before it is read.
	MinAge time.Duration
	// DeleteAfterRead unlinks a file once its fact has been written to the store.
	DeleteAfterRead bool
	// PollInterval is how often the directory is listed while the scanner runs.
	PollInterval time.Duration
	// MaxFacts bounds the in-memory fact list; the newest are kept.
	MaxFacts int
	// RetentionDays bounds how long the fact files are kept. It mirrors the observation
	// store's own retention: the facts are a sidecar of those records, so they must not
	// outlive them. Zero means the default.
	RetentionDays int
}

// withDefaults fills in everything the caller left empty, so a partially configured scanner
// still behaves like a configured one.
func (o Options) withDefaults() Options {
	if strings.TrimSpace(o.Dir) == "" {
		o.Dir = DefaultDir
	}
	if strings.TrimSpace(o.StoreDir) == "" {
		o.StoreDir = DefaultStoreDir
	}
	if o.MinAge < 0 {
		o.MinAge = DefaultMinAge
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.MaxFacts <= 0 {
		o.MaxFacts = DefaultMaxFacts
	}
	if o.RetentionDays <= 0 {
		o.RetentionDays = DefaultRetentionDays
	}
	return o
}

// Fact is one parsed request log: what the request was, which channel served it, and the
// session and timestamp the usage record has to be matched on.
//
// Every key is always present, and a field the log did not name is empty rather than defaulted,
// because an empty channel is the honest answer for a request that failed: a guessed
// "deepseek" would be indistinguishable from a real one.
type Fact struct {
	// Time is the log's own Timestamp: the request arrival, 77-90 ms before the
	// usage-reported RequestedAt the observation record carries.
	Time   time.Time `json:"time"`
	Path   string    `json:"path"`
	Method string    `json:"method"`

	// SessionID is the raw Session_id header value ("session-<uuid>"), SessionUUID the same
	// value with the "session-" prefix stripped, and HasSession records whether the client
	// sent the header at all. Clients that do not (their records show "lcp:v1:<hex>") can only
	// be joined on tokens and time, and this flag is what lets that fallback be chosen.
	SessionID   string `json:"session_id"`
	SessionUUID string `json:"session_uuid"`
	HasSession  bool   `json:"has_session"`

	// The channel. FinalProvider is what the gateway decided; ResolvedProvider is what it
	// resolved the request to; the counts describe how many attempts it took to get there.
	// All of them are empty when the upstream answered without a routing block.
	FinalProvider             string   `json:"final_provider"`
	ResolvedProvider          string   `json:"resolved_provider"`
	CanonicalSlug             string   `json:"canonical_slug"`
	OriginalModelID           string   `json:"original_model_id"`
	PinnedProvider            string   `json:"pinned_provider"`
	AffinityOutcome           string   `json:"affinity_outcome"`
	ModelAttemptCount         int      `json:"model_attempt_count"`
	TotalProviderAttemptCount int      `json:"total_provider_attempt_count"`
	FallbacksAvailable        []string `json:"fallbacks_available"`
	GatewayCost               float64  `json:"gateway_cost"`

	// Frames is how many SSE frames the response section behind this fact carried: the last
	// section that had a routing block, or, when none had one, the last section seen.
	// AttemptsSeen is how many upstream attempts the file shows (a failed one counts), and
	// HadErrorResponse whether any of them failed before the request was served.
	Frames           int  `json:"frames"`
	AttemptsSeen     int  `json:"attempts_seen"`
	HadErrorResponse bool `json:"had_error_response"`

	// SourceFile is the file this fact came from, by base name; ParsedAt is when it was read.
	SourceFile string    `json:"source_file"`
	ParsedAt   time.Time `json:"parsed_at"`
}

// HasChannel reports whether the fact carries a gateway routing block at all. False is a real
// answer — the request never got a response with one — and must never be filled in later from
// anything else.
func (f Fact) HasChannel() bool {
	return f.FinalProvider != "" || f.ResolvedProvider != "" || f.CanonicalSlug != "" ||
		f.OriginalModelID != "" || f.PinnedProvider != ""
}

// Health is the scanner's self-diagnosis. Every key is always present, so a page can render
// the disabled state without special-casing missing fields.
type Health struct {
	Enabled   bool   `json:"enabled"`
	Directory string `json:"directory"`

	// Scanned counts the files this scanner read, Parsed the ones that became a stored fact,
	// and ParseFailures the ones that did not (unreadable, unparseable, or unstorable). With
	// WithChannel and WithoutChannel, the last two split Parsed by whether the log carried a
	// channel block.
	Scanned        int64 `json:"scanned"`
	Parsed         int64 `json:"parsed"`
	WithChannel    int64 `json:"with_channel"`
	WithoutChannel int64 `json:"without_channel"`
	ParseFailures  int64 `json:"parse_failures"`

	// Deleted and DeletedBytes are what the scanner has unlinked after a successful read.
	Deleted      int64 `json:"deleted"`
	DeletedBytes int64 `json:"deleted_bytes"`

	// SkippedYoung counts the files left alone because they are younger than MinAge (CPA may
	// still be appending), SkippedGrowing the ones whose size changed between the listing and
	// the read, and SkipMainLog the times main.log was passed over.
	SkippedYoung   int64 `json:"skipped_young"`
	SkippedGrowing int64 `json:"skipped_growing"`
	// SkippedSeen counts files this scanner had already parsed in an earlier pass. It is the
	// counter that explains a delete_after_read=false deployment: files stay on disk, facts
	// do not multiply.
	SkippedSeen int64 `json:"skipped_seen"`
	// Pruned and PrunedBytes count expired fact files this scanner removed, so a page can tell
	// "nothing arrived" apart from "retention ate it".
	Pruned      int64 `json:"pruned"`
	PrunedBytes int64 `json:"pruned_bytes"`
	SkipMainLog int64 `json:"skip_main_log"`

	// LastFactAt is when the newest fact was parsed, LastError what went wrong most recently
	// (empty when nothing has), and LastErrorAt when that was.
	LastFactAt  *time.Time `json:"last_fact_at"`
	LastError   string     `json:"last_error"`
	LastErrorAt *time.Time `json:"last_error_at"`

	// PendingFiles is how many candidate files the last pass left behind: too young, grown, or
	// simply not reached. A number that only grows means the poll interval is too long for the
	// traffic.
	PendingFiles int `json:"pending_files"`
}

// Scanner polls the log directory and turns each finished file into one Fact.
type Scanner struct {
	options Options
	writer  *factWriter

	// passMu serialises passes. mu guards the counters and the fact ring, and is held only
	// briefly: a management request reading Health must not wait for a megabyte to be parsed.
	passMu sync.Mutex
	mu     sync.Mutex
	facts  []Fact
	stats  Health

	// seen remembers the files this scanner already parsed, keyed by name|size|mtime. Without
	// it a run with delete_after_read=false re-parses every file on every pass and appends the
	// same fact again: the fact store would then count one request many times, which is worse
	// than a missing row. Entries are trimmed oldest-first once the map passes seenCap.
	seenMu   sync.Mutex
	seen     map[string]struct{}
	seenKeys []string

	startMu sync.Mutex
	started bool
	stopCh  chan struct{}
	wg      sync.WaitGroup

	// nowFn and testAfterInfo/testAfterStat are test seams, in the spirit of the observation
	// recorder's own clock: without them the age gate and the two halves of the growth gate
	// could only be exercised by racing the filesystem. All three are inert in production.
	nowFn         func() time.Time
	testAfterInfo func(name string)
	testAfterStat func(name string)
}

// New builds a scanner. It does not touch the filesystem: Start and ScanOnce do.
func New(options Options) *Scanner {
	resolved := options.withDefaults()
	return &Scanner{
		options: resolved,
		writer:  &factWriter{dir: resolved.StoreDir},
		seen:    make(map[string]struct{}),
		stats:   Health{Enabled: resolved.Enabled, Directory: resolved.Dir},
		nowFn:   time.Now,
	}
}

// Start begins polling. It does nothing at all when the scanner is disabled, which is what
// keeps a switched-off feature free of both a goroutine and a directory access.
func (s *Scanner) Start() {
	if s == nil || !s.options.Enabled {
		return
	}
	s.startMu.Lock()
	if s.started {
		s.startMu.Unlock()
		return
	}
	stop := make(chan struct{})
	s.stopCh = stop
	s.started = true
	s.startMu.Unlock()

	s.wg.Add(1)
	// The channel is handed to the loop rather than read from the field: a Stop that lands
	// while the first pass is still running clears the field, and a loop that re-read it would
	// then wait on nothing forever.
	go s.loop(stop)
}

// Stop ends polling and waits for the pass in flight to finish. It is safe to call more than
// once, and safe to call on a scanner that was never started.
func (s *Scanner) Stop() {
	if s == nil {
		return
	}
	s.startMu.Lock()
	stop := s.stopCh
	started := s.started
	s.stopCh = nil
	s.started = false
	s.startMu.Unlock()
	if !started || stop == nil {
		return
	}
	close(stop)
	s.wg.Wait()
}

// loop polls until Stop. The scan is done off the request path in both directions: it runs in
// its own goroutine, and its result only ever reaches health.
func (s *Scanner) loop(stop <-chan struct{}) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.options.PollInterval)
	defer ticker.Stop()
	// The first pass runs at once: a scanner that waited a whole interval before its first look
	// would leave the view empty for no reason.
	_, _ = s.ScanOnce()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// Failures are already in health; the loop exists to survive them.
			_, _ = s.ScanOnce()
		}
	}
}

// ScanOnce runs one pass and returns how many facts were stored. It is exported for tests and
// for an operator who wants one pass on demand; the polling loop uses it too.
//
// The returned error describes the first failure of the pass. It is also in health, and it is
// never fatal: the remaining files are still read, and a caller may ignore it entirely.
// seenCap bounds the seen-set. At ~60 files a minute a full window stays well inside it; the
// trim keeps the newest entries so a long run cannot grow it without limit.
const (
	seenCap  = 20000
	seenTrim = 5000
)

func seenKey(name string, size int64, modTime time.Time) string {
	return fmt.Sprintf("%s|%d|%d", name, size, modTime.UnixNano())
}

func (s *Scanner) alreadySeen(key string) bool {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	_, ok := s.seen[key]
	return ok
}

func (s *Scanner) markSeen(key string) {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if _, ok := s.seen[key]; ok {
		return
	}
	s.seen[key] = struct{}{}
	s.seenKeys = append(s.seenKeys, key)
	if len(s.seenKeys) > seenCap {
		for _, old := range s.seenKeys[:seenTrim] {
			delete(s.seen, old)
		}
		s.seenKeys = append([]string(nil), s.seenKeys[seenTrim:]...)
	}
}

// pruneStore deletes fact files older than the retention. It is deliberately dumb and
// fail-open: the file names carry the UTC date, so expiry is a string comparison against the
// oldest kept day, and an unreadable directory only moves a health counter.
func (s *Scanner) pruneStore(now time.Time) {
	if s == nil || !s.options.Enabled || s.options.RetentionDays <= 0 {
		return
	}
	oldest := now.UTC().AddDate(0, 0, -(s.options.RetentionDays - 1)).Format("2006-01-02")
	entries, errRead := os.ReadDir(s.options.StoreDir)
	if errRead != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, factPrefix) || !strings.HasSuffix(name, factSuffix) {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, factPrefix), factSuffix)
		if len(day) != len("2006-01-02") || day >= oldest {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		if errRemove := os.Remove(filepath.Join(s.options.StoreDir, name)); errRemove != nil {
			continue
		}
		size := info.Size()
		s.count(func(health *Health) {
			health.Pruned++
			health.PrunedBytes += size
		})
	}
}

func (s *Scanner) ScanOnce() (int, error) {
	if s == nil || !s.options.Enabled {
		return 0, nil
	}
	s.passMu.Lock()
	defer s.passMu.Unlock()

	now := s.nowFn()
	entries, errRead := os.ReadDir(s.options.Dir)
	if errRead != nil {
		s.noteError(fmt.Errorf("read channel log directory: %w", errRead))
		return 0, errRead
	}
	var firstErr error
	processed := 0
	stored := 0
	// deferred counts the files left for a later pass: still being written, or too young to
	// touch. It is what pending_files means, so it is counted where those decisions are made.
	deferred := 0
	for _, entry := range entries {
		name := entry.Name()
		// A directory is never entered and a symlink is never followed: the scan is exactly
		// one level deep, over real files.
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if !strings.HasSuffix(name, logSuffix) {
			continue
		}
		if name == mainLogName {
			// CPA's own process log: not a request log, and never deleted.
			s.count(func(health *Health) { health.SkipMainLog++ })
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			s.recordFailure(errInfo)
			firstErr = firstError(firstErr, errInfo)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if age := now.Sub(info.ModTime()); age < s.options.MinAge {
			s.count(func(health *Health) { health.SkippedYoung++ })
			deferred++
			continue
		}
		// The size checked here is the one the decision was made on. It is compared with the
		// size at open time and with the bytes actually read, so a file CPA is still writing is
		// never parsed half-way, and never deleted on the strength of a partial parse.
		size := info.Size()
		if s.testAfterInfo != nil {
			s.testAfterInfo(name)
		}
		fresh, errStat := os.Stat(filepath.Join(s.options.Dir, name))
		if errStat != nil {
			s.recordFailure(errStat)
			firstErr = firstError(firstErr, errStat)
			continue
		}
		if fresh.Size() != size {
			s.count(func(health *Health) { health.SkippedGrowing++ })
			deferred++
			continue
		}
		if s.testAfterStat != nil {
			s.testAfterStat(name)
		}
		key := seenKey(name, size, info.ModTime())
		if s.alreadySeen(key) {
			s.count(func(health *Health) { health.SkippedSeen++ })
			continue
		}
		processed++
		storedFact, errFile := s.scanFile(name, size, info.ModTime(), now)
		if errFile != nil {
			s.recordFailure(errFile)
			firstErr = firstError(firstErr, errFile)
			continue
		}
		// Marked seen only after a successful parse: a file that failed to parse must be tried
		// again on the next pass rather than silently counted once.
		s.markSeen(key)
		if storedFact {
			stored++
		}
	}
	s.setPending(deferred)
	s.pruneStore(now)
	return stored, firstErr
}

// scanFile reads, persists and (when configured) unlinks one file.
func (s *Scanner) scanFile(name string, size int64, modTime, now time.Time) (bool, error) {
	path := filepath.Join(s.options.Dir, name)
	handle, errOpen := os.Open(path)
	if errOpen != nil {
		return false, fmt.Errorf("open channel log %s: %w", name, errOpen)
	}
	s.count(func(health *Health) { health.Scanned++ })
	counted := &countingReader{reader: handle}
	fact, errParse := parseLog(counted, name, now)
	errClose := handle.Close()
	if errParse != nil {
		return false, fmt.Errorf("read channel log %s: %w", name, errParse)
	}
	if errClose != nil {
		return false, fmt.Errorf("close channel log %s: %w", name, errClose)
	}
	if counted.bytes != size {
		// The file grew or was replaced while it was being read. Its parse describes only part
		// of it, so it is neither stored nor deleted: the next pass will see a quiet file.
		s.count(func(health *Health) { health.SkippedGrowing++ })
		return false, nil
	}
	if fact.Time.IsZero() {
		// No usable Timestamp in the section: the file's own mtime is the closest thing to the
		// arrival time, and it is what the 2s join window is measured against.
		fact.Time = modTime
	}
	if errStore := s.writer.append(fact); errStore != nil {
		return false, errStore
	}
	if fact.HasChannel() {
		s.count(func(health *Health) { health.WithChannel++ })
	} else {
		s.count(func(health *Health) { health.WithoutChannel++ })
	}
	s.recordFact(fact)
	if !s.options.DeleteAfterRead {
		return true, nil
	}
	// Only a file whose fact is on disk is unlinked, and only ever the one path this pass
	// built from the configured directory.
	if errRemove := os.Remove(path); errRemove != nil {
		s.noteError(fmt.Errorf("unlink channel log %s: %w", name, errRemove))
		return true, nil
	}
	s.count(func(health *Health) {
		health.Deleted++
		health.DeletedBytes += size
	})
	return true, nil
}

// Health returns a snapshot of the counters.
func (s *Scanner) Health() Health {
	if s == nil {
		return Health{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Facts returns the stored facts, newest first. Facts the scanner no longer holds are still on
// disk: the ring is a rendering convenience, not the record.
func (s *Scanner) Facts() []Fact {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	out := make([]Fact, len(s.facts))
	copy(out, s.facts)
	s.mu.Unlock()
	sort.SliceStable(out, func(first, second int) bool {
		return out[first].Time.After(out[second].Time)
	})
	return out
}

// Options returns the configuration the scanner resolved, defaults included.
func (s *Scanner) Options() Options {
	if s == nil {
		return Options{}
	}
	return s.options
}

// recordFact prepends the fact to the ring and trims it. The newest first order is by arrival
// at the scanner; Facts sorts by the log's own timestamp for rendering.
func (s *Scanner) recordFact(fact Fact) {
	at := s.nowFn()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Parsed++
	s.stats.LastFactAt = &at
	s.facts = append([]Fact{fact}, s.facts...)
	if len(s.facts) > s.options.MaxFacts {
		s.facts = s.facts[:s.options.MaxFacts]
	}
}

// count updates the counters under the lock.
func (s *Scanner) count(update func(*Health)) {
	s.mu.Lock()
	update(&s.stats)
	s.mu.Unlock()
}

// setPending publishes the backlog of the pass that just finished.
func (s *Scanner) setPending(pending int) {
	if pending < 0 {
		pending = 0
	}
	s.count(func(health *Health) { health.PendingFiles = pending })
}

// noteError records a failure in health. It never returns it to a request path: this package
// has no request path.
func (s *Scanner) noteError(err error) {
	if err == nil {
		return
	}
	at := s.nowFn()
	s.mu.Lock()
	s.stats.LastError = err.Error()
	s.stats.LastErrorAt = &at
	s.mu.Unlock()
}

// recordFailure counts a file that did not become a stored fact, and keeps the reason.
func (s *Scanner) recordFailure(err error) {
	s.count(func(health *Health) { health.ParseFailures++ })
	s.noteError(err)
}

func firstError(current, candidate error) error {
	if current != nil {
		return current
	}
	return candidate
}

// countingReader reports how many bytes were actually read, which is how a file that grew
// during the read is detected without a second stat race.
type countingReader struct {
	reader io.Reader
	bytes  int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	read, errRead := r.reader.Read(buffer)
	r.bytes += int64(read)
	return read, errRead
}
