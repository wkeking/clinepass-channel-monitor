// Package observation records, for every request the host completes, which upstream channel
// served it.
//
// The record has two dimensions, and they come from two different sources:
//
//   - cpa_provider is the CPA-side credential the usage hook reports (usageWire.Provider,
//     e.g. "openai-compatible-cline1"). The host calls usage.handle once per request, whatever
//     wire protocol the client spoke. This is what the JSONL store holds.
//   - the gateway channel (finalProvider / resolvedProvider / canonicalSlug / the attempt
//     counts / the gateway cost) belongs to the Cline gateway, and only the upstream chat
//     response names it. CPA writes that response into its per-request debug log and drops the
//     routing block when it translates the response for the client, so the channel is read out
//     of the log by internal/channellog and joined onto the record when a view is rendered
//     (join.go). A request whose log carries no channel — a failure, a client that sends no
//     Session_id header, a fact that has not been parsed yet — stays unjoined, with empty
//     channel fields. Guessing one would be indistinguishable from measuring it.
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
//     read, so the cost does not depend on how much the client streams. The join runs on the
//     read path, over a record set that is read back from the store.
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
//
//	v1  the retired stream-sniffing source
//	v2  the usage-hook source; named the CPA credential final_provider / resolved_provider
//	v3  the usage-hook source with the credential renamed cpa_provider and the gateway channel
//	    joined in at read time
//
// A v1 or v2 line still reads back: Record.UnmarshalJSON maps its final_provider /
// resolved_provider onto cpa_provider, and leaves the gateway fields empty, because a line
// written before the join existed cannot carry a channel. The page and the export therefore
// show those rows with a credential and no channel, which is what they are.
const schemaVersion = 3

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

// Record is one observed request.
//
// It is written to the JSONL store with the credential the usage hook reported and nothing
// else. The gateway half of the struct is filled by the join when the record is read for the
// /channel view or the CSV export (join.go): the stored line never carries it, which is what
// keeps "what the host said" and "what the channel log said" separable on disk.
//
// Every key the retired stream-sniffing source fed stays in the struct so a v1 line on disk
// still reads back; the keys this build has no source for are documented as such and are
// simply never set.
type Record struct {
	Schema int       `json:"v"`
	Time   time.Time `json:"time"`

	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// GenerationID came from the gateway routing block; it is not one of the fields the join
	// fills, so this build never sets it.
	GenerationID string `json:"generation_id,omitempty"`

	Model         string `json:"model,omitempty"`
	Alias         string `json:"alias,omitempty"`
	UpstreamModel string `json:"upstream_model,omitempty"`
	CanonicalSlug string `json:"canonical_slug,omitempty"`
	// OriginalModel came from the gateway routing block; it is not one of the fields the join
	// fills, so this build never sets it.
	OriginalModel string `json:"original_model_id,omitempty"`

	// CPProvider is the CPA-side credential the host reported (usageWire.Provider): the key
	// the request was served with, e.g. "openai-compatible-cline1". It is a fact about this
	// deployment, not about the Cline gateway. A v1/v2 line that named it final_provider or
	// resolved_provider reads back here (Record.UnmarshalJSON).
	CPProvider string `json:"cpa_provider,omitempty"`

	// The gateway channel, as the request log knew it. These four keys and channel_source are
	// the channel the page has always claimed to show; they are empty on every record this
	// build stores, and are filled only on the copy a view or the export reads.
	//
	//   - GatewayProvider is finalProvider: the channel that actually answered.
	//   - GatewayResolvedProvider is resolvedProvider: what the gateway resolved the request
	//     to, which differs from finalProvider when a fallback happened.
	//   - GatewaySlug is canonicalSlug, GatewayAttempts is totalProviderAttemptCount, and
	//     GatewayCost is the gateway's own cost figure.
	//   - ChannelSource says where the four came from: "log" when a fact was joined, empty when
	//     nothing was, which is the case for a failure and for a client that sent no session.
	//
	// Empty is the answer for a request that failed: it has no routing block at all, and a
	// guessed "deepseek" would be indistinguishable from a measured one.
	GatewayProvider         string  `json:"gateway_provider,omitempty"`
	GatewayResolvedProvider string  `json:"gateway_resolved_provider,omitempty"`
	GatewaySlug             string  `json:"gateway_slug,omitempty"`
	GatewayAttempts         int     `json:"gateway_attempts,omitempty"`
	GatewayCost             float64 `json:"gateway_cost,omitempty"`
	ChannelSource           string  `json:"channel_source,omitempty"`

	// FinalProvider and ResolvedProvider are the same two gateway values under the names the
	// page and the export have always used. The names belong to the real channel now: they are
	// written together with gateway_provider / gateway_resolved_provider from one fact, so the
	// old names and the new ones can never disagree.
	FinalProvider    string `json:"final_provider,omitempty"`
	ResolvedProvider string `json:"resolved_provider,omitempty"`
	// AuthID, AuthIndex and AuthType identify the credential behind the CPA credential name.
	AuthID    string `json:"auth_id,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
	AuthType  string `json:"auth_type,omitempty"`
	// ExecutorType, ReasoningEffort and ServiceTier describe how the request was executed.
	ExecutorType    string `json:"executor_type,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ServiceTier     string `json:"service_tier,omitempty"`

	// PinnedProvider, AffinityOutcome, UpstreamRequestID, Fallbacks, ModelAttempts and
	// Attempts described the gateway's own routing decisions. The usage payload carries no
	// such thing, so this build leaves them empty; the keys stay for v1 lines. The join fills
	// the separate gateway_* keys instead, so an old line and a joined one never share a key.
	PinnedProvider    string   `json:"pinned_provider,omitempty"`
	AffinityOutcome   string   `json:"affinity,omitempty"`
	UpstreamRequestID string   `json:"upstream_request_id,omitempty"`
	Fallbacks         []string `json:"fallbacks_available,omitempty"`
	ModelAttempts     int      `json:"model_attempt_count,omitempty"`
	Attempts          int      `json:"total_provider_attempt_count,omitempty"`

	// Protocol came from the client protocol the stream was read in; never set by this build,
	// because the usage hook is protocol independent.
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
	// Frames counted the chunks of a stream; never set by this build. The join does not fill
	// it either: the fact's frame count describes the log section, not the client's stream.
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
	// CostUSD and BYOK came from the gateway usage block; never set by this build. The
	// gateway's own cost of a joined request is GatewayCost, which is a different figure from
	// a different source.
	CostUSD float64 `json:"cost_usd,omitempty"`
	BYOK    bool    `json:"is_byok,omitempty"`

	// UserAgent, ClaudeCodeVersion, ClientApp and SourceFormat described the client that
	// made the call. The usage payload carries none of them; they stay for v1 lines.
	UserAgent         string `json:"user_agent,omitempty"`
	ClaudeCodeVersion string `json:"claude_code_version,omitempty"`
	ClientApp         string `json:"client_app,omitempty"`
	SourceFormat      string `json:"source_format,omitempty"`
}

// UnmarshalJSON reads one stored line into the current shape.
//
// A v1 or v2 line named the CPA credential final_provider / resolved_provider, which v3 gives
// to the gateway channel. Reading such a line without this mapping would file a credential as
// a channel and make an old window look like it had joined something. So: the old names are
// read as the credential, and the gateway fields are cleared afterwards — a line written
// before the join existed cannot carry a channel, whatever else it says.
func (record *Record) UnmarshalJSON(data []byte) error {
	// The alias keeps the decoder off this method, which would otherwise recurse.
	type storedRecord Record
	var decoded storedRecord
	if errUnmarshal := json.Unmarshal(data, &decoded); errUnmarshal != nil {
		return errUnmarshal
	}
	*record = Record(decoded)
	if record.Schema >= schemaVersion {
		return nil
	}
	if record.CPProvider == "" {
		// final_provider is the primary old name; resolved_provider carried the same value.
		record.CPProvider = firstNonEmpty(record.FinalProvider, record.ResolvedProvider)
	}
	record.FinalProvider = ""
	record.ResolvedProvider = ""
	record.GatewayProvider = ""
	record.GatewayResolvedProvider = ""
	record.GatewaySlug = ""
	record.GatewayAttempts = 0
	record.GatewayCost = 0
	record.ChannelSource = ""
	return nil
}

// Recorder accumulates observations and appends them to the JSONL store.
type Recorder struct {
	options Options
	// facts is where the gateway channel comes from (join.go). It may be nil: a deployment
	// with the request-log scanner switched off then reads every record as unjoined.
	facts FactSource

	mu      sync.Mutex
	started bool
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
	// Facts is the CPA request-log scanner the gateway channel is read from. Nil is a valid
	// configuration: every record then reads as unjoined, with an empty channel.
	//
	// It is an interface, not a cached list, because the facts are read again on every query.
	// A fact reaches the scanner seconds after the request it belongs to (the scanner only
	// reads a log file once it has stopped growing), so a list read once at startup would
	// leave every later request unjoined.
	Facts FactSource
}

// New builds a recorder. It does not touch the filesystem yet: Start does.
func New(options Options) *Recorder {
	return &Recorder{
		options: options,
		facts:   options.Facts,
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
// It is the hot path, and it is cheap: one record built from the payload and one queue send.
// It never blocks on I/O and never blocks on a full queue — a collector that slowed the proxy
// down would be worse than a missing row.
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
		CPProvider:          wire.Provider,
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

// warmUp reads the last day of records once so health can report how much history a freshly
// loaded plugin can see. Nothing is cached from it: every query reads the store itself, which
// is what lets a v1 line, a v2 line and a v3 line be read under the same rules. v1 and v2
// lines load here too — UnmarshalJSON maps their credential onto cpa_provider.
func (r *Recorder) warmUp() {
	if r.store == nil {
		return
	}
	loaded, truncated, errLoad := r.store.loadRecent(24*time.Hour, warmupByteCap, func(Record) {})
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

// RecordsSince returns the stored records whose time is inside the window, newest first, read
// from the on-disk store so a query always sees the same history a reload would. The records
// are exactly what was written: the gateway channel is not joined in here, which is what makes
// it the honest view of what the usage hook reported. ChannelRecordsSince is the joined one.
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

// ChannelRecordsSince returns the window's records as the 「渠道」 view reads them: the same
// records with the gateway channel joined in from the request-log facts. A record no fact
// matched is returned unchanged — empty channel fields, empty channel_source — so the page can
// tell "this request has no known channel" apart from "this request was on the baseline".
//
// The facts are read again on this call rather than cached: the newest request's log file has
// usually not been parsed yet when the record lands, and a cached list would keep reporting
// that gap for as long as the process runs.
func (r *Recorder) ChannelRecordsSince(window Window) ([]Record, error) {
	records, errLoad := r.RecordsSince(window)
	if errLoad != nil || len(records) == 0 {
		return records, errLoad
	}
	join := r.openJoin()
	for index, record := range records {
		if fact, matched := join.match(record); matched {
			records[index] = record.withChannel(fact)
		}
	}
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
	// sampleCap bounds the per-hour, per-provider latency samples a percentile is computed
	// from: the newest ones win, which bounds the memory of a long-running process.
	sampleCap = 240
)
