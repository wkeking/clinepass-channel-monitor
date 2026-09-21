package main

import (
	"bytes"
	"encoding/json"
	"time"
)

// identity carries the correlation keys taken from the request intercept hook.
type identity struct {
	RequestHash string
	SessionID   string
	Model       string
	Stream      bool
	Source      string
	RequestedAt time.Time
}

// event is one recorded Cline request. The struct is deliberately flat so that a
// JSONL line maps one-to-one onto the table columns of the management page.
type event struct {
	Schema  int    `json:"schema"`
	EventID string `json:"event_id"`
	// PluginVersion records which build produced the row. It makes the JSONL file
	// self-describing when a host is running several plugin versions during an upgrade.
	PluginVersion string `json:"plugin_version"`
	// Timestamp is the request time reported by the usage hook, rendered in the
	// configured timezone.
	Timestamp time.Time `json:"timestamp"`

	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	Host     string `json:"host,omitempty"`

	Model      string `json:"model"`
	ModelAlias string `json:"model_alias"`
	APIKey     string `json:"api_key"`
	SessionID  string `json:"session_id,omitempty"`
	ParentSID  string `json:"parent_session_id,omitempty"`

	AuthIndex string `json:"auth_index,omitempty"`
	AuthType  string `json:"auth_type,omitempty"`

	Failed     bool   `json:"failed"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error,omitempty"`

	LatencyMS              int64   `json:"latency_ms"`
	TTFTMS                 int64   `json:"ttft_ms"`
	TokensPerSecond        float64 `json:"tokens_per_second"`
	TokensPerSecondAfterTT float64 `json:"tokens_per_second_after_ttft"`

	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	TotalTokens     int64 `json:"total_tokens"`

	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`

	PromptCacheHitTokens  int64  `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int64  `json:"prompt_cache_miss_tokens"`
	SystemFingerprint     string `json:"system_fingerprint,omitempty"`

	FinalProvider             string `json:"final_provider"`
	ResolvedProvider          string `json:"resolved_provider"`
	CanonicalSlug             string `json:"canonical_slug"`
	OriginalModelID           string `json:"original_model_id,omitempty"`
	ModelAttemptCount         int64  `json:"model_attempt_count"`
	TotalProviderAttemptCount int64  `json:"total_provider_attempt_count"`
	FallbacksAvailableCount   int64  `json:"fallbacks_available_count"`

	Cost         string `json:"cost,omitempty"`
	InputCost    string `json:"input_cost,omitempty"`
	OutputCost   string `json:"output_cost,omitempty"`
	GenerationID string `json:"generation_id,omitempty"`

	ClientProtocol   string `json:"client_protocol,omitempty"`
	UpstreamProtocol string `json:"upstream_protocol,omitempty"`
	Stream           bool   `json:"stream"`

	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ServiceTier     string `json:"service_tier,omitempty"`

	ChannelMissing        bool   `json:"channel_missing"`
	PlanningReasoningLen  int    `json:"planning_reasoning_length,omitempty"`
	PlanningReasoningText string `json:"planning_reasoning,omitempty"`
}

// orderedEvent mirrors event for JSONL output so field order stays stable and
// readable. The wire names are identical.
type orderedEvent struct {
	Schema        int    `json:"schema"`
	EventID       string `json:"event_id"`
	PluginVersion string `json:"plugin_version"`
	Timestamp     string `json:"timestamp"`
	Provider      string `json:"provider"`
	BaseURL       string `json:"base_url"`
	Host          string `json:"host,omitempty"`
	Model         string `json:"model"`
	ModelAlias    string `json:"model_alias"`
	APIKey        string `json:"api_key"`
	SessionID     string `json:"session_id,omitempty"`
	ParentSID     string `json:"parent_session_id,omitempty"`
	AuthIndex     string `json:"auth_index,omitempty"`
	AuthType      string `json:"auth_type,omitempty"`

	Failed     bool   `json:"failed"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error,omitempty"`

	LatencyMS              int64   `json:"latency_ms"`
	TTFTMS                 int64   `json:"ttft_ms"`
	TokensPerSecond        float64 `json:"tokens_per_second"`
	TokensPerSecondAfterTT float64 `json:"tokens_per_second_after_ttft"`

	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	TotalTokens     int64 `json:"total_tokens"`

	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`

	PromptCacheHitTokens  int64  `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int64  `json:"prompt_cache_miss_tokens"`
	SystemFingerprint     string `json:"system_fingerprint,omitempty"`

	FinalProvider             string `json:"final_provider"`
	ResolvedProvider          string `json:"resolved_provider"`
	CanonicalSlug             string `json:"canonical_slug"`
	OriginalModelID           string `json:"original_model_id,omitempty"`
	ModelAttemptCount         int64  `json:"model_attempt_count"`
	TotalProviderAttemptCount int64  `json:"total_provider_attempt_count"`
	FallbacksAvailableCount   int64  `json:"fallbacks_available_count"`

	Cost         string `json:"cost,omitempty"`
	InputCost    string `json:"input_cost,omitempty"`
	OutputCost   string `json:"output_cost,omitempty"`
	GenerationID string `json:"generation_id,omitempty"`

	ClientProtocol   string `json:"client_protocol,omitempty"`
	UpstreamProtocol string `json:"upstream_protocol,omitempty"`
	Stream           bool   `json:"stream"`

	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	ServiceTier     string `json:"service_tier,omitempty"`

	ChannelMissing        bool   `json:"channel_missing"`
	PlanningReasoningLen  int    `json:"planning_reasoning_length,omitempty"`
	PlanningReasoningText string `json:"planning_reasoning,omitempty"`
}

const eventSchema = 1

// MarshalJSON renders the stable field order used in the JSONL files. Values that
// would otherwise be HTML-escaped are written verbatim.
func (e *event) MarshalJSON() ([]byte, error) {
	ordered := orderedEvent{
		Schema:        e.Schema,
		EventID:       e.EventID,
		PluginVersion: e.PluginVersion,
		Timestamp:     e.Timestamp.Format(time.RFC3339),
		Provider:      e.Provider,
		BaseURL:       e.BaseURL,
		Host:          e.Host,
		Model:         e.Model,
		ModelAlias:    e.ModelAlias,
		APIKey:        e.APIKey,
		SessionID:     e.SessionID,
		ParentSID:     e.ParentSID,
		AuthIndex:     e.AuthIndex,
		AuthType:      e.AuthType,

		Failed:     e.Failed,
		StatusCode: e.StatusCode,
		Error:      e.Error,

		LatencyMS:              e.LatencyMS,
		TTFTMS:                 e.TTFTMS,
		TokensPerSecond:        e.TokensPerSecond,
		TokensPerSecondAfterTT: e.TokensPerSecondAfterTT,

		InputTokens:     e.InputTokens,
		OutputTokens:    e.OutputTokens,
		ReasoningTokens: e.ReasoningTokens,
		TotalTokens:     e.TotalTokens,

		CachedTokens:        e.CachedTokens,
		CacheReadTokens:     e.CacheReadTokens,
		CacheCreationTokens: e.CacheCreationTokens,

		PromptCacheHitTokens:  e.PromptCacheHitTokens,
		PromptCacheMissTokens: e.PromptCacheMissTokens,
		SystemFingerprint:     e.SystemFingerprint,

		FinalProvider:             e.FinalProvider,
		ResolvedProvider:          e.ResolvedProvider,
		CanonicalSlug:             e.CanonicalSlug,
		OriginalModelID:           e.OriginalModelID,
		ModelAttemptCount:         e.ModelAttemptCount,
		TotalProviderAttemptCount: e.TotalProviderAttemptCount,
		FallbacksAvailableCount:   e.FallbacksAvailableCount,

		Cost:         e.Cost,
		InputCost:    e.InputCost,
		OutputCost:   e.OutputCost,
		GenerationID: e.GenerationID,

		ClientProtocol:   e.ClientProtocol,
		UpstreamProtocol: e.UpstreamProtocol,
		Stream:           e.Stream,

		ReasoningEffort: e.ReasoningEffort,
		ServiceTier:     e.ServiceTier,

		ChannelMissing:        e.ChannelMissing,
		PlanningReasoningLen:  e.PlanningReasoningLen,
		PlanningReasoningText: e.PlanningReasoningText,
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(ordered); errEncode != nil {
		return nil, errEncode
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// eventFilter is the query used by the management endpoints.
type eventFilter struct {
	Since   time.Duration
	Limit   int
	Offset  int
	Channel string
	Model   string
	Source  string
	Result  string
}

func (f eventFilter) matches(e *event) bool {
	if f.Channel != "" && e.FinalProvider != f.Channel {
		return false
	}
	if f.Model != "" && e.Model != f.Model && e.ModelAlias != f.Model {
		return false
	}
	if f.Source != "" && e.APIKey != f.Source {
		return false
	}
	switch f.Result {
	case "failed":
		if !e.Failed {
			return false
		}
	case "success":
		if e.Failed {
			return false
		}
	case "missing_channel":
		if !e.ChannelMissing {
			return false
		}
	}
	return true
}
