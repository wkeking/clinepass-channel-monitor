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

// The two needles below are the cheapest possible early-out: a body that carries channel
// information contains at least one of them, so a body that contains neither is rejected
// before any JSON is parsed.
var (
	// markerNeedle matches Cline's own channel evidence (provider_metadata).
	markerNeedle = []byte("provider_metadata")
	// providerNeedle matches the serving-provider field that the routes Cline does not
	// serve through its own gateway report instead of provider_metadata.
	providerNeedle = []byte(`"provider"`)
)

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
// both accepted, with a top-level provider_metadata fallback. Cline's non-streaming
// wrapper ({"success":true,"data":{…}}) is unwrapped before the fields are read.
type upstreamEnvelope struct {
	// Data holds the completion when the upstream answered through Cline's wrapper.
	Data *upstreamEnvelope `json:"data"`

	Choices []struct {
		Message *choicePayload `json:"message"`
		Delta   *choicePayload `json:"delta"`
	} `json:"choices"`
	ProviderMetadata *rawProviderMetadata `json:"provider_metadata"`
	// Provider is the serving provider that OpenRouter-shaped responses report instead
	// of provider_metadata, e.g. "baseten" or "Relace". Other upstreams reuse the key for
	// objects, so it is decoded tolerantly: one foreign shape must not fail the whole
	// decode and hide a usable provider_metadata block.
	Provider json.RawMessage `json:"provider"`
	Model    json.RawMessage `json:"model"`
	Usage    *upstreamUsage  `json:"usage"`
}

type choicePayload struct {
	ProviderMetadata *rawProviderMetadata `json:"provider_metadata"`
	Provider         json.RawMessage      `json:"provider"`
}

// rawString returns the string value of a tolerantly decoded field, or "" for any other
// JSON type.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
		return ""
	}
	return strings.TrimSpace(value)
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

// HasChannelMarker reports whether a body could contain channel information: Cline's
// provider_metadata or the serving-provider field of an OpenRouter-shaped response.
func HasChannelMarker(body []byte) bool {
	return bytes.Contains(body, markerNeedle) || bytes.Contains(body, providerNeedle)
}

// HasRoutingMarker reports whether a body contains the routing evidence required by
// require_routing_marker.
func HasRoutingMarker(body []byte) bool {
	return bytes.Contains(body, routingMarkerNeedle)
}

// HasProviderMetadata reports whether a body mentions Cline's own channel metadata. It is
// the narrower of the two needles: a body carrying it but yielding no channel value is a
// parsing anomaly worth counting, while a foreign body that merely mentions "provider" is
// not.
func HasProviderMetadata(body []byte) bool {
	return bytes.Contains(body, markerNeedle)
}

// BodyShape lists which channel fields a body mentions without exposing its content. It
// feeds the warning the hooks emit when a body matched a needle but yielded no value.
func BodyShape(body []byte) string {
	flags := make([]string, 0, 5)
	for _, needle := range []struct {
		name  string
		bytes []byte
	}{
		{"provider_metadata", markerNeedle},
		{"provider", providerNeedle},
		{"routing", routingMarkerNeedle},
		{"choices", []byte(`"choices"`)},
		{"data", []byte(`"data"`)},
	} {
		if bytes.Contains(body, needle.bytes) {
			flags = append(flags, needle.name)
		}
	}
	if len(flags) == 0 {
		return "-"
	}
	return strings.Join(flags, "|")
}

// trimSSEFrame strips the SSE "data: " prefix and surrounding whitespace from a frame.
func trimSSEFrame(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	}
	return trimmed
}

// ExtractChannelFromBody parses one upstream response body or SSE frame.
//
// The channel value prefers Cline's gateway routing (finalProvider) and falls back to the
// serving-provider field of OpenRouter-shaped responses, so every model gets a value as
// long as the upstream reported one. A nil result means "nothing observed": the body
// carries neither field, or it could not be decoded. Callers must never turn that into an
// error that reaches the client.
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
	if inner := unwrapEnvelope(&envelope); inner != nil {
		envelope = *inner
	}
	meta := channelMetadataOf(&envelope)
	if meta == nil {
		return nil
	}
	if !storePlanningReasoning {
		meta.PlanningReasoningLength = len([]rune(meta.PlanningReasoningText))
		meta.PlanningReasoningText = ""
	}
	return meta
}

// unwrapEnvelope returns the nested completion of Cline's non-streaming wrapper
// {"success":true,"data":{…}}, or nil when the body already is the completion itself.
func unwrapEnvelope(envelope *upstreamEnvelope) *upstreamEnvelope {
	if envelope == nil || envelope.Data == nil {
		return nil
	}
	if len(envelope.Choices) > 0 || envelope.ProviderMetadata != nil || len(envelope.Provider) > 0 {
		return nil
	}
	return envelope.Data
}

// channelMetadataOf builds the observation from whichever channel evidence the envelope
// carries: gateway routing first, then the serving provider.
func channelMetadataOf(envelope *upstreamEnvelope) *ChannelMetadata {
	raw := pickProviderMetadata(envelope)
	if raw != nil && raw.Gateway != nil && raw.Gateway.Routing != nil {
		meta := normalizeChannelMetadata(raw, envelope)
		if meta.FinalProvider == "" {
			// Routing without a final provider still names the serving channel.
			meta.FinalProvider = servingProvider(envelope)
		}
		return meta
	}
	provider := servingProvider(envelope)
	if provider == "" {
		return nil
	}
	meta := &ChannelMetadata{FinalProvider: provider}
	if envelope != nil {
		meta.UpstreamModel = rawString(envelope.Model)
		if envelope.Usage != nil {
			meta.UpstreamPromptTokens = envelope.Usage.PromptTokens
			meta.UpstreamCompletionTokens = envelope.Usage.CompletionTokens
			meta.UpstreamCacheCreationTokens = envelope.Usage.CacheCreationInputToken
		}
	}
	return meta
}

// servingProvider reads the provider reported by an OpenRouter-shaped envelope.
func servingProvider(envelope *upstreamEnvelope) string {
	if envelope == nil {
		return ""
	}
	if provider := rawString(envelope.Provider); provider != "" {
		return provider
	}
	for i := range envelope.Choices {
		if delta := envelope.Choices[i].Delta; delta != nil {
			if provider := rawString(delta.Provider); provider != "" {
				return provider
			}
		}
		if message := envelope.Choices[i].Message; message != nil {
			if provider := rawString(message.Provider); provider != "" {
				return provider
			}
		}
	}
	return ""
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
		meta.UpstreamModel = rawString(envelope.Model)
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
