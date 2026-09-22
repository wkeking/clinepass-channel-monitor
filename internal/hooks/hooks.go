// Package hooks implements the observation hooks CPA calls for every request: the request
// interceptor creates a correlation identity, the response normalizer reads the upstream
// channel evidence, and the usage hook joins both into one record. Every hook answers with
// an empty body, which the host reads as "keep the payload unchanged".
package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/wkeking/clinepass-channel-monitor/internal/abi"
	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostapi"
	"github.com/wkeking/clinepass-channel-monitor/internal/metadata"
	"github.com/wkeking/clinepass-channel-monitor/internal/plan"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
	"github.com/wkeking/clinepass-channel-monitor/internal/store"
)

const (
	// identityTTL bounds how long a request identity is kept after the request was seen.
	identityTTL = 5 * time.Minute
	// requestHashBytes is the length of the truncated sha256 used as the join key.
	requestHashBytes = 16
)

// currentTable joins the hooks of this plugin instance. It is replaced wholesale on load
// and on reconfigure, which is why it is read through table().
var (
	tableMu      sync.RWMutex
	currentTable = newIdentityTable()
)

// table returns the correlation table of the running instance.
func table() *identityTable {
	tableMu.RLock()
	defer tableMu.RUnlock()
	return currentTable
}

// Reset drops every correlation entry. The plugin calls it on load and on reconfigure.
func Reset() {
	tableMu.Lock()
	currentTable = newIdentityTable()
	tableMu.Unlock()
}

// InFlight reports how many request identities are being tracked, for /health.
func InFlight() int {
	return table().len()
}

// requestIdentity carries the correlation keys for one in-flight request.
type requestIdentity struct {
	Hash      string
	SessionID string
	Model     string
	Source    string
	Endpoint  string
	Stream    bool
	// RoutingSeen records whether an upstream frame carried gateway routing evidence.
	RoutingSeen bool
	// ClientProtocol and UpstreamProtocol come from the response hook.
	ClientProtocol   string
	UpstreamProtocol string
	CreatedAt        time.Time
}

// identityTable joins the three hooks of one request. The request intercept hook
// creates entries, the response hook annotates them, and the usage hook consumes them.
type identityTable struct {
	mu      sync.Mutex
	entries map[string]*requestIdentity
	order   []string
	// routing holds what the response hook saw per request, keyed by request hash. It
	// outlives the identity entry so the usage hook can still tell "the host matched but
	// this response carried no routing marker" apart from "this request was never seen".
	routing     map[string]store.RoutingState
	routingFIFO []string
}

// maxRoutedEntries bounds the routing-evidence memory.
const maxRoutedEntries = 512

func newIdentityTable() *identityTable {
	return &identityTable{
		entries: make(map[string]*requestIdentity),
		routing: make(map[string]store.RoutingState),
	}
}

// remember stores the identity captured by the request intercept hook.
func (t *identityTable) remember(entry *requestIdentity) {
	if entry == nil || entry.Hash == "" {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	existing, ok := t.entries[entry.Hash]
	if !ok {
		entry.CreatedAt = now
		t.entries[entry.Hash] = entry
		t.order = append(t.order, entry.Hash)
		t.evictLocked(now)
		return
	}
	mergeIdentity(existing, entry)
	existing.CreatedAt = now
}

func mergeIdentity(target, source *requestIdentity) {
	if target == nil || source == nil {
		return
	}
	if source.SessionID != "" {
		target.SessionID = source.SessionID
	}
	if source.Model != "" {
		target.Model = source.Model
	}
	if source.Source != "" {
		target.Source = source.Source
	}
	if source.Endpoint != "" {
		target.Endpoint = source.Endpoint
	}
	if !target.Stream {
		target.Stream = source.Stream
	}
	if !target.RoutingSeen {
		target.RoutingSeen = source.RoutingSeen
	}
	if source.ClientProtocol != "" {
		target.ClientProtocol = source.ClientProtocol
	}
	if source.UpstreamProtocol != "" {
		target.UpstreamProtocol = source.UpstreamProtocol
	}
}

// annotateProtocols records the protocol pair observed by the response hook and marks
// the routing evidence for this request.
func (t *identityTable) annotateProtocols(hash, client, upstream string, stream bool, state store.RoutingState) {
	if hash == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.markRoutingLocked(hash, state)
	entry, ok := t.entries[hash]
	if !ok {
		entry = &requestIdentity{Hash: hash, CreatedAt: time.Now()}
		t.entries[hash] = entry
		t.order = append(t.order, hash)
	}
	if state == store.RoutingConfirmed {
		entry.RoutingSeen = true
	}
	entry.ClientProtocol = client
	entry.UpstreamProtocol = upstream
	if stream {
		entry.Stream = true
	}
}

// markRoutingLocked records the strongest routing state seen for one request.
func (t *identityTable) markRoutingLocked(hash string, state store.RoutingState) {
	if existing, ok := t.routing[hash]; ok {
		if existing >= state {
			return
		}
		t.routing[hash] = state
		return
	}
	t.routing[hash] = state
	t.routingFIFO = append(t.routingFIFO, hash)
	for len(t.routingFIFO) > maxRoutedEntries {
		oldest := t.routingFIFO[0]
		t.routingFIFO = t.routingFIFO[1:]
		delete(t.routing, oldest)
	}
}

// take removes and returns the identity of a finished request.
func (t *identityTable) take(hash string) *requestIdentity {
	if hash == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[hash]
	if !ok {
		return nil
	}
	delete(t.entries, hash)
	return entry
}

// peek returns the identity of a request without removing it.
func (t *identityTable) peek(hash string) *requestIdentity {
	if hash == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.entries[hash]
}

// routingStateOf returns what the response hook observed for one request.
func (t *identityTable) routingStateOf(hash string) store.RoutingState {
	if hash == "" {
		return store.RoutingAbsent
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.routing[hash]
}

func (t *identityTable) evictLocked(now time.Time) {
	kept := t.order[:0]
	for _, hash := range t.order {
		entry, ok := t.entries[hash]
		if !ok {
			continue
		}
		if now.Sub(entry.CreatedAt) > identityTTL {
			delete(t.entries, hash)
			continue
		}
		kept = append(kept, hash)
	}
	t.order = kept
}

func (t *identityTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// requestHash identifies a request body. It is a truncated sha256 so that log lines
// and JSONL rows never expose the payload itself.
func requestHash(originalRequest []byte) string {
	if len(originalRequest) == 0 {
		return ""
	}
	sum := sha256.Sum256(originalRequest)
	return hex.EncodeToString(sum[:requestHashBytes])
}

// handleRequestInterceptBefore records the correlation keys of an in-flight request.
func RequestInterceptBefore(request []byte) ([]byte, error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			hostapi.LogAsync("error", buildinfo.ID+": request interceptor recovered from panic", map[string]string{"panic": fmt.Sprint(recovered)})
		}
	}()
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return abi.OK(pluginapi.RequestInterceptResponse{})
	}
	captured := captureIdentity(&req)
	if captured == nil {
		return abi.OK(pluginapi.RequestInterceptResponse{})
	}
	table().remember(captured)
	return abi.OK(pluginapi.RequestInterceptResponse{})
}

// handleRequestInterceptAfter records the same correlation keys after credential
// selection. The host calls it because the plugin declares the request interceptor
// capability; the plugin itself answers with an empty response, which means "leave the
// request untouched".
func RequestInterceptAfter(request []byte) ([]byte, error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			hostapi.LogAsync("error", buildinfo.ID+": request interceptor (after auth) recovered from panic", map[string]string{"panic": fmt.Sprint(recovered)})
		}
	}()
	var req pluginapi.RequestInterceptRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return abi.OK(pluginapi.RequestInterceptResponse{})
	}
	if captured := captureIdentity(&req); captured != nil {
		table().remember(captured)
	}
	// Feeds the subscription card when no other credential source is readable. The
	// value is kept in memory only, never logged and never returned to a caller.
	plan.RememberUpstreamBearer(req.Headers)
	return abi.OK(pluginapi.RequestInterceptResponse{})
}

// handleRequestComplete releases the identity of a finished request. It only runs when
// the request lifecycle capability is declared, which is not the case today, so it is
// implemented defensively rather than relied upon.
func RequestComplete(request []byte) ([]byte, error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			hostapi.LogAsync("error", buildinfo.ID+": request completion recovered from panic", map[string]string{"panic": fmt.Sprint(recovered)})
		}
	}()
	var completion pluginapi.RequestCompletion
	if errUnmarshal := json.Unmarshal(request, &completion); errUnmarshal != nil {
		return abi.OK(map[string]any{})
	}
	return abi.OK(map[string]any{})
}

func captureIdentity(req *pluginapi.RequestInterceptRequest) *requestIdentity {
	if req == nil {
		return nil
	}
	hash := requestHash(req.Body)
	if hash == "" {
		hash = strings.TrimSpace(req.RequestID)
	}
	if hash == "" {
		return nil
	}
	return &requestIdentity{
		Hash:      hash,
		SessionID: metadataString(req.Metadata, "session_id", "session", "sessionid"),
		Model:     strings.TrimSpace(req.Model),
		Source:    sourceFromMetadata(req.Metadata),
		Endpoint:  metadataString(req.Metadata, "endpoint", "path", "request_path"),
		Stream:    req.Stream,
	}
}

func sourceFromMetadata(metadata map[string]any) string {
	if value := metadataString(metadata, "source"); value != "" {
		return value
	}
	return metadataString(metadata, "client", "client_name", "user_agent")
}

// metadataString reads a metadata value by key. Keys are compared after removing
// underscores so that "session_id" also matches "sessionId".
func metadataString(metadata map[string]any, keys ...string) string {
	if len(metadata) == 0 {
		return ""
	}
	for _, key := range keys {
		normalizedKey := strings.ToLower(strings.ReplaceAll(key, "_", ""))
		for candidate, value := range metadata {
			if strings.ToLower(strings.ReplaceAll(candidate, "_", "")) != normalizedKey {
				continue
			}
			switch typed := value.(type) {
			case string:
				return strings.TrimSpace(typed)
			case []string:
				if len(typed) > 0 {
					return strings.TrimSpace(typed[0])
				}
			case float64:
				return strconv.FormatFloat(typed, 'f', -1, 64)
			}
		}
	}
	return ""
}

// handleResponseNormalizeBefore is the primary observation hook.
//
// It runs before CPA translates the upstream payload, which is the only place where
// /v1/responses traffic still carries provider_metadata. It always returns an empty
// body so the host treats the response as unmodified.
func ResponseNormalizeBefore(request []byte) ([]byte, error) {
	empty := abi.EmptyObservation
	defer func() {
		if recovered := recover(); recovered != nil {
			cfg := state.Config()
			st := state.Store()
			if cfg.Enabled && st != nil {
				st.IncParseError()
			}
			hostapi.LogAsync("error", buildinfo.ID+": response hook recovered from panic", map[string]string{
				"panic": fmt.Sprint(recovered),
			})
		}
	}()
	cfg := state.Config()
	st := state.Store()
	if !cfg.Enabled || st == nil {
		return empty, nil
	}
	var req pluginapi.ResponseTransformRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return empty, nil
	}
	hash := requestHash(req.OriginalRequest)
	if hash == "" {
		return empty, nil
	}
	identities := table()
	// Cost of the common path: one bytes.Contains over the frame.
	if !metadata.HasChannelMarker(req.Body) {
		// The response hook only sees bodies, so a response without the marker means the
		// request produced no channel evidence: exactly what marker_missing counts.
		identities.markRouting(hash, store.RoutingMarkerOnly)
		return empty, nil
	}
	routing := store.RoutingMarkerOnly
	if metadata.HasRoutingMarker(req.Body) {
		routing = store.RoutingConfirmed
	} else if cfg.RequireRoutingMark {
		// A marker without routing evidence is not a channel observation.
		identities.annotateProtocols(hash, strings.TrimSpace(req.ToFormat), strings.TrimSpace(req.FromFormat), req.Stream, store.RoutingMarkerOnly)
		return empty, nil
	}
	meta := metadata.ExtractChannelFromBody(req.Body, cfg.StorePlanningReasoning)
	if meta == nil {
		st.IncParseError()
		return empty, nil
	}
	// The identity entry is kept (not taken) so a later frame of the same request still
	// resolves the session; it expires through the identity TTL.
	identities.annotateProtocols(hash, strings.TrimSpace(req.ToFormat), strings.TrimSpace(req.FromFormat), req.Stream, routing)
	entry := identities.peek(hash)
	sessionID := ""
	model := strings.TrimSpace(req.Model)
	source := ""
	if entry != nil {
		sessionID = entry.SessionID
		source = entry.Source
		if entry.Model != "" {
			model = entry.Model
		}
	}
	meta.ClientProtocol = strings.TrimSpace(req.ToFormat)
	meta.UpstreamProtocol = strings.TrimSpace(req.FromFormat)
	meta.Stream = req.Stream
	st.AddChannel(&store.PendingChannel{
		RequestHash: hash,
		Routing:     routing,
		Identity: store.Identity{
			RequestHash: hash,
			SessionID:   sessionID,
			Model:       model,
			Source:      source,
			Stream:      req.Stream,
		},
		Meta:      meta,
		CreatedAt: time.Now(),
	})
	if cfg.LogEvents {
		hostapi.LogAsync("info", buildinfo.ID+": channel observed", map[string]string{
			"final_provider": meta.FinalProvider,
			"model":          model,
			"client_format":  req.ToFormat,
			"upstream":       req.FromFormat,
			"stream":         strconv.FormatBool(req.Stream),
			"cost":           meta.Cost,
		})
	}
	return empty, nil
}

// handleUsage turns a finished request into one recorded row.
func Usage(request []byte) ([]byte, error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			hostapi.LogAsync("error", buildinfo.ID+": usage hook recovered from panic", map[string]string{"panic": fmt.Sprint(recovered)})
		}
	}()
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(request, &record); errUnmarshal != nil {
		return abi.OK(map[string]any{})
	}
	cfg := state.Config()
	st := state.Store()
	if !cfg.Enabled || st == nil {
		return abi.OK(map[string]any{})
	}
	st.IncRequests()
	if !hostAllowed(&record, cfg, st) {
		return abi.OK(map[string]any{})
	}
	observed := st.ConsumeChannel(store.Identity{
		SessionID: strings.TrimSpace(record.SessionID),
		Model:     strings.TrimSpace(record.Model),
	}, record.RequestedAt, record.Latency)
	if !observationAccepted(&record, cfg, observed) {
		return abi.OK(map[string]any{})
	}
	var meta *metadata.ChannelMetadata
	if observed != nil {
		meta = observed.Meta
		// The observation is consumed, so its identity entry is no longer needed.
		table().take(observed.RequestHash)
	}
	e := buildEvent(&record, cfg)
	applyObservation(cfg, meta, e)
	store.Write(cfg, st, e)
	st.Add(e)
	if meta != nil {
		st.IncHostMatched()
	}
	if cfg.LogEvents {
		hostapi.LogAsync("info", buildinfo.ID+": request recorded", map[string]string{
			"model":          e.Model,
			"final_provider": e.FinalProvider,
			"failed":         strconv.FormatBool(e.Failed),
			"latency_ms":     strconv.FormatInt(e.LatencyMS, 10),
			"cost":           e.Cost,
		})
	}
	return abi.OK(map[string]any{})
}

// markRouting records the routing state of a request without touching its identity.
func (t *identityTable) markRouting(hash string, state store.RoutingState) {
	if hash == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.markRoutingLocked(hash, state)
}

// observationAccepted applies the request-level judgement from FR-2.
//
// When require_routing_marker is on, a request is only recorded if the gateway routing
// marker really appeared in its response. Two cases are deliberately still recorded:
// the usage record can be tied to a channel observation (the strongest evidence
// possible), or the request failed before producing any upstream body, in which case
// channel_missing marks the row and the failure stays visible.
func observationAccepted(record *pluginapi.UsageRecord, cfg config.Config, observed *store.PendingChannel) bool {
	st := state.Store()
	if observed != nil {
		if cfg.RequireRoutingMark && observed.Routing != store.RoutingConfirmed {
			st.IncMarkerMissing()
			return false
		}
		return true
	}
	if record.Failed {
		// Nothing to inspect: the request never produced an upstream response body.
		st.IncChannelMissing()
		return true
	}
	if cfg.RequireRoutingMark {
		// No channel observation for a successful request means its response carried no
		// routing marker, so it is not a Cline channel record.
		st.IncMarkerMissing()
		return false
	}
	st.IncChannelMissing()
	return true
}

// hostAllowed applies the row-level judgement: the usage record's base_url host must
// be one of the configured hosts. An empty hosts list disables this rule (marker-only).
func hostAllowed(record *pluginapi.UsageRecord, cfg config.Config, st *store.Store) bool {
	if len(cfg.Hosts) == 0 {
		return true
	}
	if _, matched := cfg.HostMatched(record.BaseURL); !matched {
		// Self-diagnosis: the request reached an upstream that is not in hosts. Surfaced
		// through /health so a wrong hosts list is visible instead of silent.
		st.RecordUnmatchedHost(store.UnmatchedHostSample{
			Host:     config.HostFromBaseURL(record.BaseURL),
			Provider: record.Provider,
			Model:    record.Model,
			Time:     time.Now().Format(time.RFC3339),
		})
		return false
	}
	return true
}

// buildEvent maps a usage record onto the recorded row.
func buildEvent(record *pluginapi.UsageRecord, cfg config.Config) *store.Event {
	requestedAt := record.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}
	location := timeLocation(cfg.Timezone)
	e := &store.Event{
		Schema:          store.EventSchema,
		EventID:         eventID(record),
		PluginVersion:   buildinfo.Version,
		Timestamp:       requestedAt.In(location),
		Provider:        strings.TrimSpace(record.Provider),
		BaseURL:         strings.TrimSpace(record.BaseURL),
		Model:           strings.TrimSpace(record.Model),
		ModelAlias:      strings.TrimSpace(record.Alias),
		APIKey:          apiKeyOf(record.APIKey, cfg),
		SessionID:       strings.TrimSpace(record.SessionID),
		ParentSID:       strings.TrimSpace(record.ParentSessionID),
		AuthIndex:       strings.TrimSpace(record.AuthIndex),
		AuthType:        strings.TrimSpace(record.AuthType),
		Failed:          record.Failed,
		StatusCode:      record.Failure.StatusCode,
		Error:           truncate(record.Failure.Body, 512),
		LatencyMS:       record.Latency.Milliseconds(),
		TTFTMS:          record.TTFT.Milliseconds(),
		InputTokens:     record.Detail.InputTokens,
		OutputTokens:    record.Detail.OutputTokens,
		ReasoningTokens: record.Detail.ReasoningTokens,
		TotalTokens:     record.Detail.TotalTokens,
		ReasoningEffort: strings.TrimSpace(record.ReasoningEffort),
		ServiceTier:     strings.TrimSpace(record.ServiceTier),
		ChannelMissing:  true,
	}
	e.Host = config.HostFromBaseURL(e.BaseURL)
	if cfg.CaptureCache {
		e.CachedTokens = record.Detail.CachedTokens
		e.CacheReadTokens = record.Detail.CacheReadTokens
		e.CacheCreationTokens = record.Detail.CacheCreationTokens
	}
	e.TokensPerSecond = tokensPerSecond(e.OutputTokens, record.Latency, 0)
	e.TokensPerSecondAfterTT = tokensPerSecond(e.OutputTokens, record.Latency, record.TTFT)
	if e.TotalTokens == 0 {
		e.TotalTokens = e.InputTokens + e.OutputTokens
	}
	return e
}

// applyObservation joins the channel observation with the usage record. A missing
// observation is a normal state for failed requests and is counted, not treated as an error.
func applyObservation(cfg config.Config, meta *metadata.ChannelMetadata, e *store.Event) {
	if meta == nil {
		e.ChannelMissing = true
		return
	}
	e.ChannelMissing = false
	e.FinalProvider = meta.FinalProvider
	e.ResolvedProvider = meta.ResolvedProvider
	e.CanonicalSlug = meta.CanonicalSlug
	e.OriginalModelID = meta.OriginalModelID
	e.ModelAttemptCount = meta.ModelAttemptCount
	e.TotalProviderAttemptCount = meta.TotalProviderAttemptCount
	e.FallbacksAvailableCount = meta.FallbacksAvailableCount
	e.PlanningReasoningLen = meta.PlanningReasoningLength
	e.PlanningReasoningText = meta.PlanningReasoningText
	e.ClientProtocol = meta.ClientProtocol
	e.UpstreamProtocol = meta.UpstreamProtocol
	e.Stream = meta.Stream
	if cfg.CaptureCost {
		e.Cost = meta.Cost
		e.InputCost = meta.InputCost
		e.OutputCost = meta.OutputCost
		e.GenerationID = meta.GenerationID
	}
	if cfg.CaptureCache {
		e.PromptCacheHitTokens = meta.Provider.PromptCacheHitTokens
		e.PromptCacheMissTokens = meta.Provider.PromptCacheMissTokens
		e.SystemFingerprint = meta.Provider.SystemFingerprint
	}
}

func eventID(record *pluginapi.UsageRecord) string {
	parts := []string{
		strings.TrimSpace(record.SessionID),
		strings.TrimSpace(record.Model),
		record.RequestedAt.UTC().Format(time.RFC3339Nano),
		strings.TrimSpace(record.AuthIndex),
		strings.TrimSpace(record.APIKey),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:16])
}

func apiKeyOf(key string, cfg config.Config) string {
	if cfg.MaskAPIKey {
		return metadata.MaskAPIKey(key)
	}
	return strings.TrimSpace(key)
}

// tokensPerSecond computes generation speed. When a time to first token is known the
// generation window is used, otherwise the whole latency.
func tokensPerSecond(outputTokens int64, latency, ttft time.Duration) float64 {
	if outputTokens <= 0 || latency <= 0 {
		return 0
	}
	window := latency
	if ttft > 0 && ttft < latency {
		window = latency - ttft
	}
	seconds := window.Seconds()
	if seconds <= 0 {
		return 0
	}
	return math.Round(float64(outputTokens)/seconds*100) / 100
}

func timeLocation(name string) *time.Location {
	location, errLoad := time.LoadLocation(name)
	if errLoad != nil || location == nil {
		return time.Local
	}
	return location
}

func truncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return text[:limit]
}
