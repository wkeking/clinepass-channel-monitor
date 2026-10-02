// Package observation records, for every request the host completes, which upstream channel
// served it.
//
// The source is the usage plugin hook (`usage.handle`). The host calls it once per request,
// whatever wire protocol the client spoke, and the payload names the CPA credential the
// request was routed to in Provider — which is the fact the channel ratio is computed from.
//
// It replaces an earlier design that sniffed provider_metadata.gateway.routing out of the
// streamed SSE frames. That block only ever existed on OpenAI /v1/chat/completions traffic,
// so on a host whose traffic is ~99% POST /v1/responses the collector recorded almost
// nothing; the usage hook is protocol independent and costs one call per request instead of
// one call per streamed frame.
//
// Two rules shape the implementation:
//
//   - The plugin never changes traffic. Every answer is the empty "keep" envelope, and every
//     path is fail-open: an undecodable payload, a full queue or an unwritable directory
//     leaves the host's request handling untouched.
//   - The per-request work is one JSON decode and one record build. No response byte is ever
//     read, so the cost does not depend on how much the client streams.
package observation

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"
)

// MethodUsage is the ABI method the host calls once per request. The registration declares
// the matching `usage_plugin` capability (internal/plugin/plugin.go).
const MethodUsage = "usage.handle"

// schemaVersion is written into every record so a later version can tell the shapes apart.
// v2 is the usage-hook shape; v1 lines written by the retired stream-sniffing source still
// read back, they simply lack the fields this source adds.
const schemaVersion = 2

// usageWire mirrors the payload the host hands to usage.handle. The SDK structs carry no
// json tags, so the field names below are the wire contract; the tags repeat them verbatim
// so the contract is readable in one place instead of being implied by Go's field names.
//
// APIKey and Source are credential hashes. They are decoded here only because the host sends
// them; the record never stores them.
type usageWire struct {
	Provider     string `json:"Provider"`
	BaseURL      string `json:"BaseURL"`
	ExecutorType string `json:"ExecutorType"`
	Model        string `json:"Model"`
	Alias        string `json:"Alias"`
	APIKey       string `json:"APIKey"`
	RequestID    string `json:"RequestID"`
	TraceID      string `json:"TraceID"`
	SessionID    string `json:"SessionID"`
	// ParentSessionID is the host's own lineage field; the record does not store it.
	ParentSessionID string `json:"ParentSessionID"`
	AuthID          string `json:"AuthID"`
	AuthIndex       string `json:"AuthIndex"`
	AuthType        string `json:"AuthType"`
	Source          string `json:"Source"`
	ReasoningEffort string `json:"ReasoningEffort"`
	ServiceTier     string `json:"ServiceTier"`
	// ResponseServiceTier is the tier the upstream reported back; the record keeps the
	// requested one.
	ResponseServiceTier string       `json:"ResponseServiceTier"`
	ResponseModel       string       `json:"ResponseModel"`
	Generate            bool         `json:"Generate"`
	Stream              bool         `json:"Stream"`
	Failed              bool         `json:"Failed"`
	Failure             usageFailure `json:"Failure"`
	Detail              usageDetail  `json:"Detail"`
	// RequestedAt is RFC3339 as the host serialises a time.Time.
	RequestedAt string `json:"RequestedAt"`
	// Latency and TTFT are Go time.Duration values, i.e. integer nanoseconds.
	Latency time.Duration `json:"Latency"`
	TTFT    time.Duration `json:"TTFT"`
}

// usageFailure is the host's verdict on a request that did not succeed.
type usageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

// usageDetail is the host's own token accounting for the request.
type usageDetail struct {
	InputTokens         int64 `json:"InputTokens"`
	OutputTokens        int64 `json:"OutputTokens"`
	ReasoningTokens     int64 `json:"ReasoningTokens"`
	CachedTokens        int64 `json:"CachedTokens"`
	CacheReadTokens     int64 `json:"CacheReadTokens"`
	CacheCreationTokens int64 `json:"CacheCreationTokens"`
	TotalTokens         int64 `json:"TotalTokens"`
}

// HandleUsage answers one usage.handle call: it observes the record and tells the host there
// is nothing to change.
//
// It never reports an error and never panics. A payload this build cannot decode is counted
// as a decode failure and otherwise ignored: the host's request is already finished by the
// time this hook runs, and a collector must not be able to fail a request. The copy of the
// answer is made because the host takes ownership of the bytes it is handed.
func HandleUsage(raw []byte) ([]byte, error) {
	var wire usageWire
	if errUnmarshal := json.Unmarshal(raw, &wire); errUnmarshal != nil {
		Active().noteDecodeFailure("decode usage payload: " + errUnmarshal.Error())
		return append([]byte(nil), keepAnswer...), nil
	}
	Active().ObserveUsage(&wire)
	return append([]byte(nil), keepAnswer...), nil
}

// keepAnswer is the pre-encoded "no change" answer. The host reads the usage result as an
// empty value, and encoding it once keeps the per-request path free of any JSON encoding.
var keepAnswer = []byte(`{"ok":true,"result":{}}`)

// The recorder the usage entry point feeds. It is package state rather than a parameter
// because the entry point is reached by name from the host, and because the package must not
// import the plugin package that owns the lifecycle: that would be a dependency cycle.
var (
	activeMu       sync.RWMutex
	activeRecorder *Recorder
	// inactive answers every call without doing any work, so a plugin that is loaded but has
	// observation switched off still returns a valid "no change" answer.
	inactive = &Recorder{}
)

// SetActive installs the recorder used by the usage entry point. A nil recorder leaves the
// entry point inert.
func SetActive(recorder *Recorder) {
	activeMu.Lock()
	activeRecorder = recorder
	activeMu.Unlock()
}

// Active returns the installed recorder, or one that does nothing.
func Active() *Recorder {
	activeMu.RLock()
	recorder := activeRecorder
	activeMu.RUnlock()
	if recorder == nil {
		return inactive
	}
	return recorder
}

// Record is one observed request, as appended to the JSONL store.
//
// Every key the retired stream-sniffing source fed stays in the struct so a v1 line on disk
// still reads back; the keys this build has no source for are documented as such and are
// simply never set.
type Record struct {
	Schema int       `json:"v"`
	Time   time.Time `json:"time"`

	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// GenerationID came from the gateway routing block; never set for a v2 record.
	GenerationID string `json:"generation_id,omitempty"`

	Model         string `json:"model,omitempty"`
	Alias         string `json:"alias,omitempty"`
	UpstreamModel string `json:"upstream_model,omitempty"`
	CanonicalSlug string `json:"canonical_slug,omitempty"`
	// OriginalModel came from the gateway routing block; never set for a v2 record.
	OriginalModel string `json:"original_model_id,omitempty"`

	// FinalProvider and ResolvedProvider are both the credential the host reported
	// (usageWire.Provider): the channel a request landed on.
	FinalProvider    string `json:"final_provider,omitempty"`
	ResolvedProvider string `json:"resolved_provider,omitempty"`
	// AuthID, AuthIndex and AuthType identify the credential behind the channel name.
	AuthID    string `json:"auth_id,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
	AuthType  string `json:"auth_type,omitempty"`
	// ExecutorType, ReasoningEffort and ServiceTier describe how the request was executed.
	ExecutorType    string `json:"executor_type,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ServiceTier     string `json:"service_tier,omitempty"`

	// PinnedProvider, AffinityOutcome, UpstreamRequestID, Fallbacks, ModelAttempts and
	// Attempts described the gateway's own routing decisions. The usage payload carries no
	// such thing, so a v2 record leaves them empty; the keys stay for v1 lines.
	PinnedProvider    string   `json:"pinned_provider,omitempty"`
	AffinityOutcome   string   `json:"affinity,omitempty"`
	UpstreamRequestID string   `json:"upstream_request_id,omitempty"`
	Fallbacks         []string `json:"fallbacks_available,omitempty"`
	ModelAttempts     int      `json:"model_attempt_count,omitempty"`
	Attempts          int      `json:"total_provider_attempt_count,omitempty"`

	// Protocol came from the client protocol the stream was read in; never set for a v2
	// record, because the usage hook is protocol independent.
	Protocol string `json:"protocol,omitempty"`
	Stream   bool   `json:"stream"`
	// Failed is the host's own verdict on the request.
	Failed     bool `json:"failed"`
	StatusCode int  `json:"status_code,omitempty"`

	// TTFTMs is the host's time to first token; DurationMs its total latency. Both are
	// rounded to whole milliseconds from the nanoseconds the payload carries. DecodeMs is the
	// remainder, so it excludes the wait before the first token.
	TTFTMs     int64   `json:"ttft_ms"`
	DurationMs int64   `json:"duration_ms"`
	DecodeMs   int64   `json:"decode_ms,omitempty"`
	TPS        float64 `json:"tokens_per_second,omitempty"`
	// Frames counted the chunks of a stream; never set for a v2 record.
	Frames int `json:"frames,omitempty"`

	InputTokens     int64 `json:"input_tokens,omitempty"`
	OutputTokens    int64 `json:"output_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	CachedTokens    int64 `json:"cached_tokens,omitempty"`
	// CacheReadTokens and CacheCreationTokens are the provider's own cache counters, kept
	// apart from CachedTokens because the host reports them separately.
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	TotalTokens         int64 `json:"total_tokens,omitempty"`
	// CostUSD and BYOK came from the gateway usage block; never set for a v2 record.
	CostUSD float64 `json:"cost_usd,omitempty"`
	BYOK    bool    `json:"is_byok,omitempty"`

	// UserAgent, ClaudeCodeVersion, ClientApp and SourceFormat described the client that
	// made the call. The usage payload carries none of them; they stay for v1 lines.
	UserAgent         string `json:"user_agent,omitempty"`
	ClaudeCodeVersion string `json:"claude_code_version,omitempty"`
	ClientApp         string `json:"client_app,omitempty"`
	SourceFormat      string `json:"source_format,omitempty"`
}

// Recorder accumulates observations and appends them to the JSONL store.
type Recorder struct {
	options Options

	mu      sync.Mutex
	started bool
	agg     *aggregate
	stats   Health

	queue  chan Record
	syncCh chan chan struct{}
	store  *store
	nowFn  func() time.Time
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// Options is the subset of the plugin configuration the recorder needs.
type Options struct {
	Enabled       bool
	Directory     string
	RetentionDays int
	MaxSizeMB     int
	Baseline      string
}

// New builds a recorder. It does not touch the filesystem yet: Start does.
func New(options Options) *Recorder {
	return &Recorder{
		options: options,
		agg:     newAggregate(),
		queue:   make(chan Record, queueCapacity),
		syncCh:  make(chan chan struct{}),
		nowFn:   time.Now,
		stopCh:  make(chan struct{}),
	}
}

// Start opens the store, warms the aggregates up from recent records and starts the writer
// and the retention loop.
func (r *Recorder) Start() {
	if r == nil {
		return
	}
	r.store = openStore(r.options)
	snapshot := r.store.healthSnapshot()
	r.mu.Lock()
	r.stats.Directory = snapshot.Directory
	r.stats.Files = snapshot.Files
	r.stats.Bytes = snapshot.Bytes
	if snapshot.LastError != "" {
		r.stats.LastError = snapshot.LastError
		r.stats.LastErrorAt = timePointer(r.now())
	}
	r.mu.Unlock()

	r.warmUp()
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	r.wg.Add(2)
	go r.writeLoop()
	go r.cleanLoop()
}

// Stop flushes and closes the store. It is safe to call more than once.
func (r *Recorder) Stop() {
	if r == nil || r.store == nil {
		return
	}
	select {
	case <-r.stopCh:
		return
	default:
	}
	close(r.stopCh)
	r.wg.Wait()
	r.store.close()
	r.mu.Lock()
	r.started = false
	r.mu.Unlock()
}

// ObserveUsage is the single ingestion entry point: one call per completed request, whatever
// wire protocol the client spoke.
//
// It is the hot path, and it is cheap: one record built from the payload, one aggregate
// update, one queue send. It never blocks on I/O and never blocks on a full queue — a
// collector that slowed the proxy down would be worse than a missing row.
func (r *Recorder) ObserveUsage(wire *usageWire) {
	if r == nil || wire == nil {
		return
	}
	r.mu.Lock()
	started := r.started
	r.mu.Unlock()
	if !started {
		return
	}
	record := buildRecord(wire, r.now())

	r.mu.Lock()
	r.agg.add(record, r.options.Baseline)
	r.stats.Events++
	if record.Failed {
		r.stats.FailedEvents++
	}
	r.stats.LastRecordAt = timePointer(record.Time)
	if len(r.queue) == cap(r.queue) {
		r.stats.Dropped++
		r.mu.Unlock()
		return
	}
	r.stats.Queued++
	r.mu.Unlock()

	select {
	case r.queue <- record:
	default:
		r.mu.Lock()
		r.stats.Dropped++
		r.stats.Queued--
		r.mu.Unlock()
	}
}

// buildRecord maps one usage payload onto the stored shape. now is the recorder clock, used
// only when the payload's own timestamp cannot be read.
func buildRecord(wire *usageWire, now time.Time) Record {
	record := Record{
		Schema:              schemaVersion,
		Time:                stampOf(wire.RequestedAt, now),
		RequestID:           firstNonEmpty(wire.RequestID, wire.TraceID),
		SessionID:           wire.SessionID,
		Model:               wire.Model,
		Alias:               wire.Alias,
		UpstreamModel:       wire.ResponseModel,
		CanonicalSlug:       wire.ResponseModel,
		FinalProvider:       wire.Provider,
		ResolvedProvider:    wire.Provider,
		AuthID:              wire.AuthID,
		AuthIndex:           wire.AuthIndex,
		AuthType:            wire.AuthType,
		ExecutorType:        wire.ExecutorType,
		ReasoningEffort:     wire.ReasoningEffort,
		ServiceTier:         wire.ServiceTier,
		Stream:              wire.Stream,
		Failed:              wire.Failed,
		StatusCode:          statusCodeOf(wire),
		InputTokens:         wire.Detail.InputTokens,
		OutputTokens:        wire.Detail.OutputTokens,
		ReasoningTokens:     wire.Detail.ReasoningTokens,
		CachedTokens:        wire.Detail.CachedTokens,
		CacheReadTokens:     wire.Detail.CacheReadTokens,
		CacheCreationTokens: wire.Detail.CacheCreationTokens,
		TotalTokens:         wire.Detail.TotalTokens,
		TTFTMs:              millis(wire.TTFT),
		DurationMs:          millis(wire.Latency),
	}
	if record.TotalTokens == 0 {
		record.TotalTokens = record.InputTokens + record.OutputTokens
	}
	record.DecodeMs = record.DurationMs - record.TTFTMs
	if record.DecodeMs < 0 {
		// The host measures the two clocks separately, so a payload may report a first token
		// later than the total latency. The remainder cannot be negative.
		record.DecodeMs = 0
	}
	// Decode speed counts the tokens the model actually generated, reasoning included,
	// because that is what the client waited for. Below minDecodeWindowMs the window is too
	// short to measure anything, so the record carries no speed instead of a number that would
	// pull the page's percentiles up.
	if record.DecodeMs >= minDecodeWindowMs && record.OutputTokens > 0 {
		record.TPS = float64(record.OutputTokens) / (float64(record.DecodeMs) / 1000)
	}
	return record
}

// statusCodeOf picks the status the record reports: the host's own failure code when the
// request failed, and 200 for everything else. A success that reports no status at all is
// still a success; a failure that carries 0 keeps the 0, because inventing a code there
// would hide what the host actually said.
func statusCodeOf(wire *usageWire) int {
	if wire.Failed {
		return wire.Failure.StatusCode
	}
	return http.StatusOK
}

// stampOf parses the host's RFC3339 timestamp and falls back to the recorder clock, so a
// record always carries a usable time.
func stampOf(raw string, fallback time.Time) time.Time {
	if parsed, errParse := time.Parse(time.RFC3339Nano, raw); errParse == nil {
		return parsed.UTC()
	}
	return fallback.UTC()
}

// millis rounds a duration to whole milliseconds.
func millis(value time.Duration) int64 {
	return int64(math.Round(float64(value) / float64(time.Millisecond)))
}

// firstNonEmpty returns the first value that carries something.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func timePointer(value time.Time) *time.Time {
	stamp := value
	return &stamp
}

func (r *Recorder) now() time.Time {
	if r == nil || r.nowFn == nil {
		return time.Now()
	}
	return r.nowFn()
}

// noteDecodeFailure counts a payload this build could not decode and keeps the last reason.
// A payload written by a host build this plugin does not know is exactly what the counter
// exists for; it is never a reason to fail the call.
func (r *Recorder) noteDecodeFailure(message string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stats.DecodeFailures++
	r.stats.LastDecodeError = message
	stamp := r.now()
	r.stats.LastDecodeErrorAt = &stamp
	r.mu.Unlock()
}

// --- writer, retention and warmup ------------------------------------------------

func (r *Recorder) writeLoop() {
	defer r.wg.Done()
	// The writer buffers, so a quiet stream would otherwise sit in the buffer until a burst
	// fills it. A page load must not have to wait that long to see the last request.
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			r.drain()
			return
		case record := <-r.queue:
			r.append(record)
		case ack := <-r.syncCh:
			r.drainQueue()
			_ = r.store.flush()
			close(ack)
		case <-ticker.C:
			_ = r.store.flush()
		}
	}
}

// Flush waits until every queued record has reached the file. The hot path is asynchronous
// by design, so a reader that must see the record it just observed (a test, or the records
// table on a page load) needs this instead of guessing a delay.
func (r *Recorder) Flush() {
	if r == nil || r.store == nil {
		return
	}
	ack := make(chan struct{})
	select {
	case r.syncCh <- ack:
	case <-r.stopCh:
		return
	}
	select {
	case <-ack:
	case <-r.stopCh:
	}
}

// drainQueue appends everything already queued without touching the buffer.
func (r *Recorder) drainQueue() {
	for {
		select {
		case record := <-r.queue:
			r.append(record)
		default:
			return
		}
	}
}

func (r *Recorder) drain() {
	r.drainQueue()
	_ = r.store.flush()
}

func (r *Recorder) append(record Record) {
	errWrite := r.store.append(record)
	r.mu.Lock()
	defer r.mu.Unlock()
	if errWrite != nil {
		r.stats.WriteFailures++
		r.stats.LastError = errWrite.Error()
		stamp := r.now()
		r.stats.LastErrorAt = &stamp
		return
	}
	r.stats.Written++
	r.stats.LastWriteAt = timePointer(record.Time)
}

// cleanLoop enforces the retention window and the directory size cap.
func (r *Recorder) cleanLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer func() {
		ticker.Stop()
		r.wg.Done()
	}()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			snapshot := r.store.clean(r.now())
			r.mu.Lock()
			r.stats.Files = snapshot.Files
			r.stats.Bytes = snapshot.Bytes
			if snapshot.LastError != "" {
				r.stats.LastError = snapshot.LastError
			}
			r.mu.Unlock()
		}
	}
}

// warmUp rebuilds the in-memory aggregates from the records still on disk, so a plugin
// reload does not blank the page for the length of the window. v1 lines load here too: the
// reader ignores the keys a v2 record no longer fills.
func (r *Recorder) warmUp() {
	if r.store == nil {
		return
	}
	loaded, truncated, errLoad := r.store.loadRecent(24*time.Hour, warmupByteCap, func(record Record) {
		r.agg.add(record, r.options.Baseline)
	})
	r.mu.Lock()
	r.stats.Warmup = Warmup{Records: loaded, Truncated: truncated}
	if errLoad != nil {
		r.stats.LastError = errLoad.Error()
	}
	r.mu.Unlock()
}

const warmupByteCap = 96 << 20

// Health is the collector's self-description, surfaced through /health.
type Health struct {
	Enabled   bool   `json:"enabled"`
	Directory string `json:"directory"`

	// Events counts the usage records this collector accepted; FailedEvents how many of them
	// the host reported as failed. DecodeFailures counts payloads this build could not read
	// at all, which is the one counter that explains a plugin that sees calls but stores
	// nothing.
	Events         int64 `json:"events"`
	FailedEvents   int64 `json:"failed_events"`
	DecodeFailures int64 `json:"decode_failures"`
	Dropped        int64 `json:"dropped"`
	Written        int64 `json:"written"`
	Queued         int64 `json:"queued"`
	WriteFailures  int64 `json:"write_failures"`

	LastRecordAt *time.Time `json:"last_record_at,omitempty"`
	LastWriteAt  *time.Time `json:"last_write_at,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	LastErrorAt  *time.Time `json:"last_error_at,omitempty"`

	LastDecodeError   string     `json:"last_decode_error,omitempty"`
	LastDecodeErrorAt *time.Time `json:"last_decode_error_at,omitempty"`

	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
	Warmup Warmup `json:"warmup"`
}

// Warmup describes how much of the on-disk history a freshly loaded plugin managed to read.
type Warmup struct {
	Records   int  `json:"records"`
	Truncated bool `json:"truncated"`
}

// Health returns a snapshot of the collector state.
func (r *Recorder) Health() Health {
	if r == nil {
		return Health{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	health := r.stats
	health.Enabled = r.options.Enabled
	if r.store != nil {
		snapshot := r.store.healthSnapshot()
		health.Directory = snapshot.Directory
		health.Files = snapshot.Files
		health.Bytes = snapshot.Bytes
	}
	return health
}

// RecordsSince returns the stored records whose time is inside the window, newest first,
// read from the on-disk store so a query always sees the same history a reload would.
func (r *Recorder) RecordsSince(window Window) ([]Record, error) {
	if r == nil || r.store == nil {
		return nil, nil
	}
	since := r.now().Add(-window.Duration())
	records := make([]Record, 0, 256)
	_, _, errLoad := r.store.loadSince(since, func(record Record) {
		records = append(records, record)
	})
	if errLoad != nil {
		return records, errLoad
	}
	sort.Slice(records, func(first, second int) bool {
		return records[first].Time.After(records[second].Time)
	})
	return records, nil
}

// --- limits ---------------------------------------------------------------------

const (
	queueCapacity = 4096
	// minDecodeWindowMs is the shortest window that can measure a decode speed at all. The host
	// hands over a short answer in a single batch (live traffic: 14 tokens over 4 ms, i.e.
	// 2800 t/s), and a number like that is a batching artifact, not a speed.
	minDecodeWindowMs = 50
	// flushInterval is how long a written record may sit in the writer buffer before it
	// reaches the file: the page's records table is never more than this far behind.
	flushInterval = 2 * time.Second
	// bucketSlots is how many hourly buckets are kept: a week, the longest window.
	bucketSlots = 24 * 7
	// sampleCap bounds the per-bucket, per-provider latency samples percentiles are
	// computed from: the newest ones win.
	sampleCap = 240
)
