// Package observation records, for every streamed request, which upstream channel the
// Cline gateway actually used.
//
// Cline reports that in the response body: the last chunk of a stream carries
// provider_metadata.gateway.routing with finalProvider, resolvedProvider, canonicalSlug,
// the attempt counters and the client session id. None of it is visible anywhere else on
// this host — it is not in the usage records, not in CPA's own logs, and not derivable
// from the model name — so the only way to keep it is to observe the response stream.
//
// Two rules shape the implementation:
//
//   - The plugin never changes traffic. Every interceptor answer is an empty response,
//     and every path is fail-open: unparseable bytes, a full queue, an unwritable
//     directory or a panic all leave the client stream untouched.
//   - The per-chunk work is a few byte scans. JSON is decoded only for the frames that
//     can carry what we need (the routing frame, the usage frame, and possibly the first
//     text frame); everything else is one map lookup and a timestamp.
package observation

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ChunkHeaderInitIndex mirrors pluginapi.StreamChunkHeaderInitIndex. The host calls the
// interceptor once with this index before the first payload chunk, carrying the request
// headers and the upstream response headers.
const ChunkHeaderInitIndex = -1

// MethodStreamChunk is the ABI method the host calls for every streamed response chunk.
const MethodStreamChunk = "response.intercept_stream_chunk"

// schemaVersion is written into every record so a later version can tell the shapes apart.
const schemaVersion = 1

// InterceptRequest mirrors the host's pluginapi.StreamChunkInterceptRequest. The SDK type
// carries no json tags, so these field names are the wire contract and must not be renamed.
type InterceptRequest struct {
	RequestID       string         `json:"RequestID"`
	SourceFormat    string         `json:"SourceFormat"`
	Model           string         `json:"Model"`
	RequestedModel  string         `json:"RequestedModel"`
	RequestHeaders  http.Header    `json:"RequestHeaders"`
	ResponseHeaders http.Header    `json:"ResponseHeaders"`
	Body            []byte         `json:"Body"`
	ChunkIndex      int            `json:"ChunkIndex"`
	Metadata        map[string]any `json:"Metadata"`
}

// InterceptResponse mirrors pluginapi.StreamChunkInterceptResponse. Every field keeps its
// zero value: an empty Body means "keep this chunk" and empty Headers means "keep the
// response headers", so answering this can never alter what the client receives.
type InterceptResponse struct {
	Headers      http.Header `json:"Headers,omitempty"`
	Body         []byte      `json:"Body,omitempty"`
	ClearHeaders []string    `json:"ClearHeaders,omitempty"`
	DropChunk    bool        `json:"DropChunk,omitempty"`
}

// Handle answers one response.intercept_stream_chunk call: it observes the chunk and tells
// the host to keep it.
//
// It never reports an error. A chunk this plugin cannot decode is not a reason to disturb a
// running stream, and the answer it returns is the "no change" envelope, which the host
// reads as "keep the current payload". The copy is made because the host takes ownership of
// the bytes it is handed.
func Handle(raw []byte) ([]byte, error) {
	var request InterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &request); errUnmarshal != nil {
		return append([]byte(nil), keepChunk...), nil
	}
	Active().Observe(&request)
	return append([]byte(nil), keepChunk...), nil
}

// keepChunk is the pre-encoded "keep everything" answer. Encoding it once keeps the
// per-chunk path free of any JSON encoding: the host only reads the fields it cares about.
var keepChunk = []byte(`{"ok":true,"result":{}}`)

// The recorder the interceptor entry point feeds. It is package state rather than a
// parameter because the entry point is reached by name from the host, and because the
// package must not import the plugin package that owns the lifecycle: that would be a
// dependency cycle.
var (
	activeMu       sync.RWMutex
	activeRecorder *Recorder
	// inactive answers every call without doing any work, so a plugin that is loaded but
	// has observation switched off still returns a valid "keep the chunk" answer.
	inactive = &Recorder{}
)

// SetActive installs the recorder used by the interceptor entry point. A nil recorder
// leaves the entry point inert.
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
type Record struct {
	Schema int       `json:"v"`
	Time   time.Time `json:"time"`

	RequestID    string `json:"request_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	GenerationID string `json:"generation_id,omitempty"`

	// Model is the model the client asked for; UpstreamModel is what the gateway resolved
	// it to, and CanonicalSlug is the provider-qualified name behind it.
	Model         string `json:"model,omitempty"`
	UpstreamModel string `json:"upstream_model,omitempty"`
	CanonicalSlug string `json:"canonical_slug,omitempty"`
	OriginalModel string `json:"original_model_id,omitempty"`

	FinalProvider    string `json:"final_provider,omitempty"`
	ResolvedProvider string `json:"resolved_provider,omitempty"`
	PinnedProvider   string `json:"pinned_provider,omitempty"`
	AffinityOutcome  string `json:"affinity,omitempty"`
	// UpstreamRequestID is the gateway's own id for the upstream call (providerRequestId).
	// It is the only handle that ties a record to a gateway-side support ticket.
	UpstreamRequestID string   `json:"upstream_request_id,omitempty"`
	Fallbacks         []string `json:"fallbacks_available,omitempty"`

	ModelAttempts int `json:"model_attempt_count,omitempty"`
	Attempts      int `json:"total_provider_attempt_count,omitempty"`

	Protocol   string `json:"protocol,omitempty"`
	Stream     bool   `json:"stream"`
	StatusCode int    `json:"status_code,omitempty"`

	// TTFTMs is measured from the moment the upstream response headers reached the host to
	// the first chunk carrying text, so it excludes the connection and queueing time the
	// gateway spent before the response began.
	TTFTMs     int64   `json:"ttft_ms"`
	DurationMs int64   `json:"duration_ms"`
	DecodeMs   int64   `json:"decode_ms,omitempty"`
	TPS        float64 `json:"tokens_per_second,omitempty"`
	Frames     int     `json:"frames"`

	InputTokens     int64   `json:"input_tokens,omitempty"`
	OutputTokens    int64   `json:"output_tokens,omitempty"`
	ReasoningTokens int64   `json:"reasoning_tokens,omitempty"`
	CachedTokens    int64   `json:"cached_tokens,omitempty"`
	TotalTokens     int64   `json:"total_tokens,omitempty"`
	CostUSD         float64 `json:"cost_usd,omitempty"`
	BYOK            bool    `json:"is_byok,omitempty"`

	UserAgent         string `json:"user_agent,omitempty"`
	ClaudeCodeVersion string `json:"claude_code_version,omitempty"`
	ClientApp         string `json:"client_app,omitempty"`
	SourceFormat      string `json:"source_format,omitempty"`
}

// pendingStream is the in-flight state of one streamed request. The host gives us
// per-chunk headers but no session of our own, so we key on the request id it repeats for
// every chunk of the same response.
type pendingStream struct {
	started   time.Time
	lastFrame time.Time
	firstText time.Time

	model        string
	upstream     string
	protocol     string
	userAgent    string
	claudeVer    string
	clientApp    string
	responseCode int
	frames       int

	routing  routingBlock
	hasRoute bool
	usage    usageBlock
	hasUsage bool

	// previous is the chunk before this one. The host hands over whole SSE frames today,
	// but a frame split across two chunks is a real possibility in a streaming pipeline,
	// and provider_metadata sits in the largest frame of the stream, so a cheap
	// "re-decode with the previous chunk prepended" retry is worth keeping.
	previous []byte
	textSeen bool

	// retryRouting and retryUsage carry "the needle was seen in an earlier chunk but the
	// frame did not decode yet" into the following chunk, so a frame split across two
	// chunks is still read. Both are bounded by maxDecodeRetries.
	retryRouting int
	retryUsage   int

	// routingGivenUp and usageGivenUp end the retry loop once the budget is spent. They
	// matter: a needle that never resolves would otherwise keep every remaining chunk of the
	// stream on the decode path, and a stream that reports no channel at all is exactly the
	// case this plugin has to stay cheap on.
	routingGivenUp bool
	usageGivenUp   bool
}

// routingBlock is the part of provider_metadata.gateway.routing this plugin keeps.
type routingBlock struct {
	Final            string
	Resolved         string
	Pinned           string
	Affinity         string
	CanonicalSlug    string
	OriginalModel    string
	UpstreamModel    string
	UpstreamRequest  string
	GenerationID     string
	SessionID        string
	Fallbacks        []string
	ModelAttempts    int
	ProviderAttempts int
	StatusCode       int
}

// usageBlock is the token and cost part of a chunk.
type usageBlock struct {
	Input     int64
	Output    int64
	Reasoning int64
	Cached    int64
	Total     int64
	Cost      float64
	BYOK      bool
}

// Recorder accumulates observations and appends them to the JSONL store.
type Recorder struct {
	options Options

	mu      sync.Mutex
	started bool
	pending map[string]*pendingStream
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
		pending: make(map[string]*pendingStream),
		agg:     newAggregate(),
		queue:   make(chan Record, queueCapacity),
		syncCh:  make(chan chan struct{}),
		nowFn:   time.Now,
		stopCh:  make(chan struct{}),
	}
}

// Start opens the store, warms the aggregates up from recent records and starts the
// writer, the janitor and the permissions check.
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
	r.wg.Add(3)
	go r.writeLoop()
	go r.janitorLoop()
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

// Observe feeds one interceptor call. It is the hot path: everything it does per chunk is
// a slice scan and a map lookup, and it never blocks on I/O.
func (r *Recorder) Observe(request *InterceptRequest) {
	if r == nil || request == nil {
		return
	}
	r.mu.Lock()
	started := r.started
	r.mu.Unlock()
	if !started {
		return
	}
	if request.ChunkIndex == ChunkHeaderInitIndex {
		r.begin(request)
		return
	}
	r.chunk(request)
}

// begin handles the header-init call: the one place the client request headers are
// available (schema v3+ omits them from every payload chunk).
func (r *Recorder) begin(request *InterceptRequest) {
	stream := newStream(request, r.now())
	r.mu.Lock()
	if len(r.pending) > maxPendingStreams {
		// A stream that never delivered a routing block and was never cleaned up: dropping
		// the oldest tracking state keeps the map bounded without touching any stream.
		for key, stale := range r.pending {
			if r.now().Sub(stale.started) > pendingTTL {
				delete(r.pending, key)
			}
		}
	}
	r.pending[request.RequestID] = stream
	r.mu.Unlock()
}

// newStream opens the tracking state of one request. A stream opened from a payload chunk
// instead of the header-init call carries no client headers, which is why the request
// headers are read here and nowhere else.
func newStream(request *InterceptRequest, started time.Time) *pendingStream {
	stream := &pendingStream{
		started:   started,
		model:     strings.TrimSpace(request.Model),
		protocol:  request.SourceFormat,
		userAgent: headerValue(request.RequestHeaders, "User-Agent"),
		claudeVer: claudeCodeVersion(request.RequestHeaders),
		clientApp: firstHeader(request.RequestHeaders, "X-App", "X-Title", "X-Client-Name"),
	}
	if stream.model == "" {
		stream.model = strings.TrimSpace(request.RequestedModel)
	}
	if status := headerValue(request.ResponseHeaders, "X-Upstream-Status"); status != "" {
		stream.responseCode = atoi(status)
	}
	return stream
}

// chunk handles one payload chunk.
//
// The work per chunk is: two byte scans for the routing and usage needles, one scan for the
// loose text needles while the first token is still unknown, a map lookup and a timestamp.
// JSON is decoded only when a scan hits, and the hit rate is one or two frames per stream.
func (r *Recorder) chunk(request *InterceptRequest) {
	body := request.Body
	if len(body) == 0 {
		return
	}
	// The two needle scans are what keeps the cost of a chunk at three byte scans: JSON is
	// decoded only for the one or two frames per stream that carry the channel or the usage.
	hasRoutingNeedle := bytes.Contains(body, needleRouting)
	hasUsageNeedle := bytes.Contains(body, needleUsage)

	r.mu.Lock()
	stream := r.pending[request.RequestID]
	if stream == nil {
		// The header-init call normally opens the stream, but a missing one must not lose a
		// request: the plugin can be enabled while a response is already streaming, and the
		// ratio this collector exists for is only exact if every request is counted. The
		// exception is a frame that carries nothing observable — the terminator of a stream
		// that was already recorded — because opening a request for it would invent one.
		if !hasRoutingNeedle && !hasUsageNeedle && !containsAny(body, textNeedles) {
			r.mu.Unlock()
			return
		}
		stream = newStream(request, r.now())
		r.pending[request.RequestID] = stream
	}
	stream.frames++
	stream.lastFrame = r.now()
	// A needle can straddle two chunks, in which case the chunk that carries it cannot be
	// decoded on its own. The retry counters carry that state into the following chunk.
	wantsRouting := (!stream.hasRoute && !stream.routingGivenUp && hasRoutingNeedle) || stream.retryRouting > 0
	wantsUsage := (!stream.hasUsage && !stream.usageGivenUp && hasUsageNeedle) || stream.retryUsage > 0
	wantsText := !stream.textSeen
	needsRoute := !stream.hasRoute
	previous := stream.previous
	stream.previous = nil
	if !stream.hasRoute && len(body) <= maxPreviousChunk {
		stream.previous = body
	}
	r.mu.Unlock()

	if wantsText {
		// The loose text needles match almost every frame of a stream, so this scan is only
		// worth running while the first token is still unknown.
		wantsText = containsAny(body, textNeedles)
	}
	if needsRoute && !hasRoutingNeedle && containsAny(body, terminatorNeedles) {
		// The stream ended without reporting a channel, so it can never become a record: the
		// Responses API is the main source of these, because its frames carry no
		// provider_metadata at all. Counting the request here, on its terminator, keeps the
		// pending count honest and the unresolved counter live instead of pendingTTL late.
		r.abandon(request.RequestID, stream)
		return
	}
	if !wantsRouting && !wantsUsage && !wantsText {
		return
	}

	frame, errParse := decodeFrame(body)
	usedPrevious := false
	if errParse != nil && len(previous) > 0 {
		// Prepending the previous chunk is how a needle frame cut in two is recovered, but
		// the decoder reads only the first complete value, which can be the earlier chunk's
		// own frame. A frame obtained this way therefore says nothing about the chunk in
		// hand, and the needle-miss shortcut below must not be applied to it.
		usedPrevious = true
		frame, errParse = decodeFrame(append(append(make([]byte, 0, len(previous)+len(body)), previous...), body...))
	}
	if errParse != nil {
		if wantsRouting || wantsUsage {
			r.noteParseFailure("decode stream chunk: " + errParse.Error())
			r.retryLater(request.RequestID, stream, wantsRouting, wantsUsage)
		}
		return
	}

	if wantsRouting {
		if routing, ok := extractRouting(frame); ok {
			r.mu.Lock()
			stream.routing = routing
			stream.hasRoute = true
			stream.retryRouting = 0
			r.mu.Unlock()
		} else if !usedPrevious && findObject(frame, "provider_metadata") == nil {
			// The needle matched nothing but the model's own text: the frame decoded cleanly
			// and holds no provider_metadata object at any depth, so this is not a routing
			// block split across chunks. Spending the retry budget here would be wrong --
			// three mentions of the word in one answer would give the stream up and lose its
			// record for good -- so only the miss counter sees it.
			r.noteNeedleMiss()
		} else if stream.retryRouting >= maxDecodeRetries {
			// The frame carried a provider_metadata object, the boundary retries are spent,
			// and there is still no routing block: record the one diagnostic that says the
			// stream had channel metadata this plugin could not read, then stop paying for it.
			r.mu.Lock()
			stream.routingGivenUp = true
			stream.retryRouting = 0
			r.mu.Unlock()
			r.noteParseFailure("provider_metadata present but no routing block")
		} else {
			r.retryLater(request.RequestID, stream, true, false)
		}
	}
	if wantsUsage {
		if usage, ok := extractUsage(frame); ok {
			r.mu.Lock()
			stream.usage = usage
			stream.hasUsage = true
			stream.retryUsage = 0
			r.mu.Unlock()
		} else if !usedPrevious && findObject(frame, "usage") == nil {
			// Same false positive as above: a frame that mentions the word but carries no
			// usage object is not a fragmented one.
			r.noteNeedleMiss()
		} else if stream.retryUsage >= maxDecodeRetries {
			// A usage object is optional, so giving up on it is not a failure worth
			// counting: it only means this stream contributes no token numbers.
			r.mu.Lock()
			stream.usageGivenUp = true
			stream.retryUsage = 0
			r.mu.Unlock()
		} else {
			r.retryLater(request.RequestID, stream, false, true)
		}
	}
	if wantsText && extractText(frame) {
		r.mu.Lock()
		if !stream.textSeen {
			stream.textSeen = true
			stream.firstText = r.now()
		}
		r.mu.Unlock()
	}
	if stream.hasRoute {
		r.finish(request, stream)
	}
}

// retryLater asks the following chunk to try again for a needle that did not decode yet.
// The attempt count is bounded so a stream that keeps producing unparseable frames cannot
// turn a bounded amount of work into an unbounded one.
func (r *Recorder) retryLater(requestID string, stream *pendingStream, routing, usage bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.pending[requestID]; !ok || current != stream {
		return
	}
	if routing && stream.retryRouting < maxDecodeRetries {
		stream.retryRouting++
	}
	if usage && stream.retryUsage < maxDecodeRetries {
		stream.retryUsage++
	}
}

// finish turns a stream whose routing block arrived into a record and hands it to the
// writer. The routing block is the last meaningful frame of a Cline stream, so this is
// where the request is complete.
func (r *Recorder) finish(request *InterceptRequest, stream *pendingStream) {
	r.mu.Lock()
	if current, ok := r.pending[request.RequestID]; !ok || current != stream {
		r.mu.Unlock()
		return
	}
	delete(r.pending, request.RequestID)
	record := buildRecord(stream, request)
	r.agg.add(record, r.options.Baseline)
	r.stats.Resolved++
	r.stats.LastRecordAt = timePointer(record.Time)
	r.stats.LastRoutingAt = timePointer(record.Time)
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

// buildRecord merges the collected pieces into the stored form.
func buildRecord(stream *pendingStream, request *InterceptRequest) Record {
	record := Record{
		Schema:            schemaVersion,
		Time:              stream.started.UTC(),
		RequestID:         request.RequestID,
		SessionID:         stream.routing.SessionID,
		GenerationID:      stream.routing.GenerationID,
		Model:             stream.model,
		UpstreamModel:     stream.routing.UpstreamModel,
		CanonicalSlug:     stream.routing.CanonicalSlug,
		OriginalModel:     stream.routing.OriginalModel,
		FinalProvider:     stream.routing.Final,
		ResolvedProvider:  stream.routing.Resolved,
		PinnedProvider:    stream.routing.Pinned,
		AffinityOutcome:   stream.routing.Affinity,
		UpstreamRequestID: stream.routing.UpstreamRequest,
		Fallbacks:         stream.routing.Fallbacks,
		ModelAttempts:     stream.routing.ModelAttempts,
		Attempts:          stream.routing.ProviderAttempts,
		Protocol:          stream.protocol,
		SourceFormat:      stream.protocol,
		Stream:            true,
		StatusCode:        stream.routing.StatusCode,
		Frames:            stream.frames,
		UserAgent:         stream.userAgent,
		ClaudeCodeVersion: stream.claudeVer,
		ClientApp:         stream.clientApp,
	}
	if record.StatusCode == 0 {
		record.StatusCode = stream.responseCode
	}
	if record.Model == "" {
		record.Model = request.RequestedModel
	}
	if !stream.lastFrame.IsZero() && stream.lastFrame.After(stream.started) {
		record.DurationMs = stream.lastFrame.Sub(stream.started).Milliseconds()
	}
	if !stream.firstText.IsZero() && stream.firstText.After(stream.started) {
		record.TTFTMs = stream.firstText.Sub(stream.started).Milliseconds()
		record.DecodeMs = record.DurationMs - record.TTFTMs
	}
	usage := stream.usage
	record.InputTokens = usage.Input
	record.OutputTokens = usage.Output
	record.ReasoningTokens = usage.Reasoning
	record.CachedTokens = usage.Cached
	record.TotalTokens = usage.Total
	record.CostUSD = usage.Cost
	record.BYOK = usage.BYOK
	if record.TotalTokens == 0 {
		record.TotalTokens = record.InputTokens + record.OutputTokens
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

// --- frame decoding -------------------------------------------------------------

var (
	needleRouting = []byte("provider_metadata")
	needleUsage   = []byte(`"usage"`)
	// terminatorNeedles mark the end of a stream. They are only scanned while a stream is
	// still unresolved, so the cost lands on the streams that would otherwise sit in the
	// pending map until pendingTTL expires.
	terminatorNeedles = [][]byte{
		[]byte(`[DONE]`),
		[]byte(`"response.completed"`),
		[]byte(`"response.incomplete"`),
		[]byte(`"response.failed"`),
	}
	// textNeedles are deliberately loose: they only gate a decode while the first text of
	// the stream is still unknown, and a false positive costs one small decode, while a
	// false negative would leave the ttft of that request empty. They cover the OpenAI chat
	// protocol (delta.content), the reasoning models (delta.reasoning), and Anthropic's
	// shape, which the host may still be speaking on the downstream side.
	textNeedles = [][]byte{
		[]byte(`"content"`),
		[]byte(`"reasoning"`),
		[]byte(`"reasoning_content"`),
		[]byte(`"text"`),
	}
)

func containsAny(body []byte, needles [][]byte) bool {
	for _, needle := range needles {
		if bytes.Contains(body, needle) {
			return true
		}
	}
	return false
}

// decodeFrame decodes one chunk into a navigation map. An SSE frame may carry the JSON
// after a "data:" prefix and may be followed by more events; json.Decoder stops after the
// first complete value, which makes that trailing content harmless.
func decodeFrame(body []byte) (map[string]any, error) {
	payload := body
	if index := bytes.Index(payload, []byte("data:")); index >= 0 {
		payload = payload[index+len("data:"):]
	}
	payload = bytes.TrimLeft(payload, " \t")
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	frame := make(map[string]any)
	if errDecode := decoder.Decode(&frame); errDecode != nil {
		return nil, errDecode
	}
	return frame, nil
}

// extractRouting finds provider_metadata.gateway.routing anywhere in the frame: the exact
// nesting depends on the protocol the client spoke (the chat protocol puts it inside
// choices[0].delta), and the one thing that must not happen is missing the only field that
// answers the question this plugin exists for.
func extractRouting(frame map[string]any) (routingBlock, bool) {
	metadata := findObject(frame, "provider_metadata")
	if metadata == nil {
		return routingBlock{}, false
	}
	gateway := mapValue(metadata, "gateway")
	if gateway == nil {
		return routingBlock{}, false
	}
	routing := mapValue(gateway, "routing")
	if routing == nil {
		return routingBlock{}, false
	}
	block := routingBlock{
		Final:            stringValue(routing, "finalProvider"),
		Resolved:         stringValue(routing, "resolvedProvider"),
		CanonicalSlug:    stringValue(routing, "canonicalSlug"),
		OriginalModel:    stringValue(routing, "originalModelId"),
		GenerationID:     stringValue(routing, "generationId"),
		SessionID:        stringValue(routing, "clientSessionId"),
		ModelAttempts:    intValue(routing, "modelAttemptCount"),
		ProviderAttempts: intValue(routing, "totalProviderAttemptCount"),
		UpstreamModel:    stringValue(routing, "model"),
	}
	if affinity := mapValue(routing, "affinity"); affinity != nil {
		block.Affinity = stringValue(affinity, "outcome")
		block.Pinned = stringValue(affinity, "pinnedProvider")
	}
	for _, name := range stringSliceValue(routing, "fallbacksAvailable") {
		block.Fallbacks = append(block.Fallbacks, name)
	}
	if attempts, ok := routing["modelAttempts"].([]any); ok {
		for _, entry := range attempts {
			attempt := asMap(entry)
			if attempt == nil {
				continue
			}
			if block.UpstreamModel == "" {
				block.UpstreamModel = stringValue(attempt, "modelId")
			}
			providers, _ := attempt["providerAttempts"].([]any)
			for _, providerEntry := range providers {
				provider := asMap(providerEntry)
				if provider == nil {
					continue
				}
				if block.UpstreamRequest == "" {
					block.UpstreamRequest = stringValue(provider, "providerRequestId")
				}
				if status := intValue(provider, "statusCode"); status != 0 && block.StatusCode == 0 {
					block.StatusCode = status
				}
			}
		}
	}
	if block.Final == "" && block.Resolved == "" {
		return routingBlock{}, false
	}
	return block, true
}

// extractUsage reads the token and cost counters of a frame. Both the OpenAI chat shape and
// the responses shape are handled, because the gateway varies them per client protocol.
func extractUsage(frame map[string]any) (usageBlock, bool) {
	usage := mapValue(frame, "usage")
	if usage == nil {
		// The gateway also ships the counters inside the same provider_metadata blob on some
		// protocols, so fall back to a search of the frame.
		usage = findObject(frame, "usage")
	}
	if usage == nil {
		return usageBlock{}, false
	}
	block := usageBlock{
		Input:     int64Value(usage, "prompt_tokens"),
		Output:    int64Value(usage, "completion_tokens"),
		Total:     int64Value(usage, "total_tokens"),
		Cached:    int64Value(usage, "cache_creation_input_tokens"),
		Cost:      floatValue(usage, "cost"),
		BYOK:      boolValue(usage, "is_byok"),
		Reasoning: 0,
	}
	if block.Input == 0 {
		block.Input = int64Value(usage, "input_tokens")
	}
	if block.Output == 0 {
		block.Output = int64Value(usage, "output_tokens")
	}
	if details := mapValue(usage, "prompt_tokens_details"); details != nil {
		if cached := int64Value(details, "cached_tokens"); cached != 0 {
			block.Cached = cached
		}
	}
	if details := mapValue(usage, "input_tokens_details"); details != nil {
		if cached := int64Value(details, "cached_tokens"); cached != 0 {
			block.Cached = cached
		}
	}
	if details := mapValue(usage, "completion_tokens_details"); details != nil {
		block.Reasoning = int64Value(details, "reasoning_tokens")
	}
	if details := mapValue(usage, "output_tokens_details"); details != nil {
		if reasoning := int64Value(details, "reasoning_tokens"); reasoning != 0 {
			block.Reasoning = reasoning
		}
	}
	if block.Total == 0 && block.Input == 0 && block.Output == 0 {
		return usageBlock{}, false
	}
	return block, true
}

// extractText reports whether a frame carries model output, which is what starts the clock
// for the first token.
func extractText(frame map[string]any) bool {
	if text := stringValue(frame, "text"); text != "" {
		return true
	}
	choices, ok := frame["choices"].([]any)
	if !ok {
		return false
	}
	for _, entry := range choices {
		choice := asMap(entry)
		if choice == nil {
			continue
		}
		for _, holder := range []map[string]any{mapValue(choice, "delta"), mapValue(choice, "message"), choice} {
			if holder == nil {
				continue
			}
			for _, key := range []string{"content", "reasoning", "reasoning_content", "text"} {
				if value := stringValue(holder, key); value != "" {
					return true
				}
			}
		}
	}
	return false
}

// --- small map helpers ----------------------------------------------------------

func asMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	mapped, _ := value.(map[string]any)
	return mapped
}

// maxSearchDepth bounds the recursive search for a key: the frames that carry the channel
// are a few hundred bytes, but the walk stays bounded so a deeply nested non-Cline payload
// cannot turn one probe hit into a long traversal.
const maxSearchDepth = 6

// findObject returns the first object stored under key anywhere in the frame. Cline reports
// provider_metadata inside choices[0].delta for the chat protocol, and the nesting differs
// per protocol, so a fixed path from the root would miss the only field this plugin exists
// for. Only frames that already matched a byte probe reach this search.
func findObject(frame map[string]any, key string) map[string]any {
	return findObjectAt(frame, key, 0)
}

func findObjectAt(value any, key string, depth int) map[string]any {
	if depth > maxSearchDepth {
		return nil
	}
	switch typed := value.(type) {
	case map[string]any:
		if found := asMap(typed[key]); found != nil {
			return found
		}
		for _, nested := range typed {
			if found := findObjectAt(nested, key, depth+1); found != nil {
				return found
			}
		}
	case []any:
		for _, nested := range typed {
			if found := findObjectAt(nested, key, depth+1); found != nil {
				return found
			}
		}
	}
	return nil
}

func mapValue(frame map[string]any, key string) map[string]any {
	if frame == nil {
		return nil
	}
	return asMap(frame[key])
}

func stringValue(frame map[string]any, key string) string {
	if frame == nil {
		return ""
	}
	value, _ := frame[key].(string)
	return strings.TrimSpace(value)
}

func boolValue(frame map[string]any, key string) bool {
	if frame == nil {
		return false
	}
	value, _ := frame[key].(bool)
	return value
}

func intValue(frame map[string]any, key string) int {
	if frame == nil {
		return 0
	}
	switch value := frame[key].(type) {
	case json.Number:
		parsed, errParse := value.Int64()
		if errParse != nil {
			return 0
		}
		return int(parsed)
	case float64:
		return int(value)
	case int:
		return value
	}
	return 0
}

// int64Value is the token-counter variant of intValue. Token counts are stored as int64
// because they are summed over a whole window.
func int64Value(frame map[string]any, key string) int64 {
	if frame == nil {
		return 0
	}
	switch value := frame[key].(type) {
	case json.Number:
		parsed, errParse := value.Int64()
		if errParse != nil {
			return 0
		}
		return parsed
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	}
	return 0
}

func floatValue(frame map[string]any, key string) float64 {
	if frame == nil {
		return 0
	}
	switch value := frame[key].(type) {
	case json.Number:
		parsed, errParse := value.Float64()
		if errParse != nil {
			return 0
		}
		return parsed
	case float64:
		return value
	case int:
		return float64(value)
	}
	return 0
}

func stringSliceValue(frame map[string]any, key string) []string {
	if frame == nil {
		return nil
	}
	raw, ok := frame[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		if text, isText := entry.(string); isText && text != "" {
			out = append(out, text)
		}
	}
	return out
}

// --- request header helpers -----------------------------------------------------

func headerValue(headers http.Header, key string) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get(key))
}

func firstHeader(headers http.Header, keys ...string) string {
	for _, key := range keys {
		if value := headerValue(headers, key); value != "" {
			return value
		}
	}
	return ""
}

// claudeCodeVersion picks up the header Claude Code sends about itself. The exact spelling
// varies between clients, so every known one is probed and the first hit wins.
func claudeCodeVersion(headers http.Header) string {
	if headers == nil {
		return ""
	}
	for name, values := range headers {
		lowered := strings.ToLower(name)
		if strings.Contains(lowered, "claude") && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	for _, key := range []string{"User-Agent", "X-App-Version", "X-Client-Version"} {
		if value := headerValue(headers, key); strings.Contains(strings.ToLower(value), "claude-code") {
			return value
		}
	}
	return ""
}

func atoi(raw string) int {
	value := 0
	for _, character := range raw {
		if character < '0' || character > '9' {
			return 0
		}
		value = value*10 + int(character-'0')
	}
	return value
}

func timePointer(value time.Time) *time.Time {
	stamp := value
	return &stamp
}

// --- aggregation ----------------------------------------------------------------

const (
	queueCapacity = 4096
	// maxPendingStreams and pendingTTL bound the in-flight map: a stream that never
	// delivers a routing block (a non-Cline upstream, an aborted request) is forgotten
	// instead of being kept for the lifetime of the process.
	maxPendingStreams = 4096
	pendingTTL        = 15 * time.Minute
	// maxPreviousChunk bounds the retained chunk used for the split-frame retry.
	maxPreviousChunk = 1 << 20
	// maxDecodeRetries bounds how many following chunks may keep trying to decode a frame
	// whose needle was already seen, so an unparseable stream cannot multiply the work.
	maxDecodeRetries = 3
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

func (r *Recorder) now() time.Time {
	if r.nowFn == nil {
		return time.Now()
	}
	return r.nowFn()
}

func (r *Recorder) noteParseFailure(message string) {
	r.mu.Lock()
	r.stats.ParseFailures++
	r.stats.LastParseError = message
	stamp := r.now()
	r.stats.LastParseErrorAt = &stamp
	r.mu.Unlock()
}

// nonStreamRequest counts a request that produced a response but no routing block: the
// upstream did not report a channel, so there is nothing to add to the ratio.
func (r *Recorder) noteUnresolved() {
	r.mu.Lock()
	r.stats.Unresolved++
	r.mu.Unlock()
}

// noteNeedleMiss counts a chunk whose probe hit was the model writing the needle word rather
// than a frame carrying the object. It is the one counter that explains a high unresolved
// count cheaply: the Responses API never sends provider_metadata at all.
func (r *Recorder) noteNeedleMiss() {
	r.mu.Lock()
	r.stats.NeedleMisses++
	r.mu.Unlock()
}

// abandon forgets one stream whose response ended without a routing block. It is the
// terminator-driven counterpart of sweepPending: the request counts as unresolved as soon as
// its stream ends instead of pendingTTL later, so the pending count stays meaningful.
func (r *Recorder) abandon(requestID string, stream *pendingStream) {
	r.mu.Lock()
	current, ok := r.pending[requestID]
	if !ok || current != stream {
		r.mu.Unlock()
		return
	}
	delete(r.pending, requestID)
	r.mu.Unlock()
	r.noteUnresolved()
}

// sweepPending forgets the streams that never completed: a request whose response ended
// without a routing block reported no channel, so it is counted as unresolved rather than
// guessed at. It returns how many were forgotten.
func (r *Recorder) sweepPending() int {
	cutoff := r.now().Add(-pendingTTL)
	unresolved := 0
	r.mu.Lock()
	for key, stream := range r.pending {
		if stream.started.Before(cutoff) {
			delete(r.pending, key)
			unresolved++
		}
	}
	r.mu.Unlock()
	for index := 0; index < unresolved; index++ {
		r.noteUnresolved()
	}
	return unresolved
}

// janitorLoop forgets streams that never completed and records them as unresolved.
func (r *Recorder) janitorLoop() {
	ticker := time.NewTicker(time.Minute)
	defer func() {
		ticker.Stop()
		r.wg.Done()
	}()
	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.sweepPending()
		}
	}
}

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
// reload does not blank the page for the length of the window.
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

	Resolved      int64 `json:"resolved"`
	Unresolved    int64 `json:"unresolved"`
	ParseFailures int64 `json:"parse_failures"`
	// NeedleMisses counts chunks that mentioned a needle word but carried no such object:
	// the model writing "provider_metadata" in its own answer, not a frame for this plugin.
	// A high count against a low resolved count usually means the traffic is the Responses
	// API, whose frames never carry routing metadata at all.
	NeedleMisses   int64 `json:"needle_misses"`
	Dropped        int64 `json:"dropped"`
	Written        int64 `json:"written"`
	Queued         int64 `json:"queued"`
	WriteFailures  int64 `json:"write_failures"`
	PendingStreams int   `json:"pending_streams"`

	LastRecordAt     *time.Time `json:"last_record_at,omitempty"`
	LastRoutingAt    *time.Time `json:"last_routing_at,omitempty"`
	LastWriteAt      *time.Time `json:"last_write_at,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	LastErrorAt      *time.Time `json:"last_error_at,omitempty"`
	LastParseError   string     `json:"last_parse_error,omitempty"`
	LastParseErrorAt *time.Time `json:"last_parse_error_at,omitempty"`

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
	health.PendingStreams = len(r.pending)
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
