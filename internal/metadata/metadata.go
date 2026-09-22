// Package metadata reads the upstream channel evidence out of a response body.
//
// Cline's gateway reports the channel it picked in provider_metadata.gateway.routing; CPA
// passes the response to the plugins before it translates it to the client protocol, which
// is the only moment that evidence is still present.
package metadata

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// markerNeedle is the cheapest possible early-out: every payload that carries channel
// information contains this byte sequence, so a body without it is rejected before
// any JSON is parsed.
var markerNeedle = []byte("provider_metadata")

// routingMarkerNeedle is the hard evidence that a request really carried gateway
// routing information: provider_metadata.gateway.routing.
var routingMarkerNeedle = []byte(`"routing"`)

// ProviderInfo holds the cache counters reported for the serving provider.
type ProviderInfo struct {
	PromptCacheHitTokens  int64  `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int64  `json:"prompt_cache_miss_tokens"`
	SystemFingerprint     string `json:"system_fingerprint,omitempty"`
}

// ChannelMetadata is the normalized channel observation extracted from one upstream body.
//
// Only counts are kept from fallbacksAvailable and modelAttempts: the tables would
// otherwise grow without bound for no diagnostic benefit.
type ChannelMetadata struct {
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

	PlanningReasoningText   string `json:"-"`
	PlanningReasoningLength int    `json:"planning_reasoning_length,omitempty"`

	Provider ProviderInfo `json:"provider"`

	// ClientProtocol and UpstreamProtocol are the protocol pair the host reported for
	// this observation, and Stream records whether the client asked for streaming.
	// They come from the response hook, which is the only hook that sees both.
	ClientProtocol   string `json:"client_protocol,omitempty"`
	UpstreamProtocol string `json:"upstream_protocol,omitempty"`
	Stream           bool   `json:"stream"`

	UpstreamModel               string `json:"upstream_model,omitempty"`
	UpstreamPromptTokens        int64  `json:"upstream_prompt_tokens,omitempty"`
	UpstreamCompletionTokens    int64  `json:"upstream_completion_tokens,omitempty"`
	UpstreamCacheCreationTokens int64  `json:"upstream_cache_creation_tokens,omitempty"`
}

// upstreamEnvelope is the subset of an upstream completion payload that this plugin
// reads. Streaming (choices[].delta) and non-streaming (choices[].message) shapes are
// both accepted, with a top-level provider_metadata fallback.
type upstreamEnvelope struct {
	Choices []struct {
		Message *choicePayload `json:"message"`
		Delta   *choicePayload `json:"delta"`
	} `json:"choices"`
	ProviderMetadata *rawProviderMetadata `json:"provider_metadata"`
	Model            string               `json:"model"`
	Usage            *upstreamUsage       `json:"usage"`
}

type choicePayload struct {
	ProviderMetadata *rawProviderMetadata `json:"provider_metadata"`
}

type upstreamUsage struct {
	PromptTokens            int64   `json:"prompt_tokens"`
	CompletionTokens        int64   `json:"completion_tokens"`
	CacheCreationInputToken int64   `json:"cache_creation_input_tokens"`
	GatewayCost             float64 `json:"gateway_cost"`
}

// rawProviderMetadata decodes provider_metadata with whichever case the upstream uses.
type rawProviderMetadata struct {
	Gateway        *rawGateway
	ProviderBlocks map[string]map[string]interface{}
}

func (r *rawProviderMetadata) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &raw); errUnmarshal != nil {
		return errUnmarshal
	}
	blocks := make(map[string]map[string]interface{}, len(raw))
	for key, value := range raw {
		if key == "gateway" {
			var gateway rawGateway
			if errGateway := json.Unmarshal(value, &gateway); errGateway == nil {
				r.Gateway = &gateway
			}
			continue
		}
		var block map[string]interface{}
		if errBlock := json.Unmarshal(value, &block); errBlock != nil {
			continue
		}
		blocks[key] = block
	}
	r.ProviderBlocks = blocks
	return nil
}

type rawGateway struct {
	Cost                string      `json:"cost"`
	InputInferenceCost  string      `json:"inputInferenceCost"`
	OutputInferenceCost string      `json:"outputInferenceCost"`
	GenerationID        string      `json:"generationId"`
	Routing             *rawRouting `json:"routing"`
}

type rawRouting struct {
	FinalProvider             string   `json:"finalProvider"`
	ResolvedProvider          string   `json:"resolvedProvider"`
	CanonicalSlug             string   `json:"canonicalSlug"`
	OriginalModelID           string   `json:"originalModelId"`
	ModelAttemptCount         int64    `json:"modelAttemptCount"`
	TotalProviderAttemptCount int64    `json:"totalProviderAttemptCount"`
	FallbacksAvailable        []string `json:"fallbacksAvailable"`
	PlanningReasoning         string   `json:"planningReasoning"`
}

// hasChannelMarker reports whether a body could contain channel information.
func HasChannelMarker(body []byte) bool {
	return bytes.Contains(body, markerNeedle)
}

// hasRoutingMarker reports whether a body contains the routing evidence required by
// require_routing_marker.
func HasRoutingMarker(body []byte) bool {
	return bytes.Contains(body, routingMarkerNeedle)
}

// trimSSEFrame strips the SSE "data: " prefix and surrounding whitespace from a frame.
func trimSSEFrame(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	}
	return trimmed
}

// extractChannelFromBody parses one upstream response body or SSE frame.
//
// A nil result means "nothing observed" - either the frame carries no channel
// metadata or it could not be decoded. Callers must never turn that into an error
// that reaches the client.
func ExtractChannelFromBody(body []byte, storePlanningReasoning bool) *ChannelMetadata {
	if !HasChannelMarker(body) {
		return nil
	}
	payload := trimSSEFrame(body)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	var envelope upstreamEnvelope
	if errUnmarshal := json.Unmarshal(payload, &envelope); errUnmarshal != nil {
		return nil
	}
	raw := pickProviderMetadata(&envelope)
	if raw == nil || raw.Gateway == nil || raw.Gateway.Routing == nil {
		return nil
	}
	meta := normalizeChannelMetadata(raw, &envelope)
	if !storePlanningReasoning {
		meta.PlanningReasoningLength = len([]rune(meta.PlanningReasoningText))
		meta.PlanningReasoningText = ""
	}
	return meta
}

func pickProviderMetadata(envelope *upstreamEnvelope) *rawProviderMetadata {
	for i := range envelope.Choices {
		choice := &envelope.Choices[i]
		if choice.Delta != nil && choice.Delta.ProviderMetadata != nil {
			return choice.Delta.ProviderMetadata
		}
		if choice.Message != nil && choice.Message.ProviderMetadata != nil {
			return choice.Message.ProviderMetadata
		}
	}
	return envelope.ProviderMetadata
}

func normalizeChannelMetadata(raw *rawProviderMetadata, envelope *upstreamEnvelope) *ChannelMetadata {
	routing := raw.Gateway.Routing
	meta := &ChannelMetadata{
		FinalProvider:             routing.FinalProvider,
		ResolvedProvider:          routing.ResolvedProvider,
		CanonicalSlug:             routing.CanonicalSlug,
		OriginalModelID:           routing.OriginalModelID,
		ModelAttemptCount:         routing.ModelAttemptCount,
		TotalProviderAttemptCount: routing.TotalProviderAttemptCount,
		FallbacksAvailableCount:   int64(len(routing.FallbacksAvailable)),
		Cost:                      raw.Gateway.Cost,
		InputCost:                 raw.Gateway.InputInferenceCost,
		OutputCost:                raw.Gateway.OutputInferenceCost,
		GenerationID:              raw.Gateway.GenerationID,
		PlanningReasoningText:     routing.PlanningReasoning,
	}
	if envelope != nil {
		meta.UpstreamModel = envelope.Model
		if envelope.Usage != nil {
			meta.UpstreamPromptTokens = envelope.Usage.PromptTokens
			meta.UpstreamCompletionTokens = envelope.Usage.CompletionTokens
			meta.UpstreamCacheCreationTokens = envelope.Usage.CacheCreationInputToken
			if meta.Cost == "" && envelope.Usage.GatewayCost != 0 {
				meta.Cost = FormatCost(envelope.Usage.GatewayCost)
			}
		}
	}
	if meta.FinalProvider != "" {
		meta.Provider = providerBlockOf(raw, meta.FinalProvider)
	}
	return meta
}

// providerBlockOf reads provider_metadata.<finalProvider>, which holds the cache
// counters reported by the serving provider.
func providerBlockOf(raw *rawProviderMetadata, provider string) ProviderInfo {
	block, ok := raw.ProviderBlocks[provider]
	if !ok {
		return ProviderInfo{}
	}
	info := ProviderInfo{
		PromptCacheHitTokens:  int64Of(block["promptCacheHitTokens"]),
		PromptCacheMissTokens: int64Of(block["promptCacheMissTokens"]),
	}
	if fingerprint, okString := block["systemFingerprint"].(string); okString {
		info.SystemFingerprint = fingerprint
	}
	return info
}

func int64Of(value interface{}) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	default:
		return 0
	}
}

// formatCost renders a numeric cost without scientific notation, matching the string
// form the gateway itself uses in provider_metadata.
func FormatCost(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// maskAPIKey keeps only the first and last four characters of a key.
func MaskAPIKey(key string) string {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 12 {
		return "****"
	}
	return trimmed[:4] + "****" + trimmed[len(trimmed)-4:]
}
