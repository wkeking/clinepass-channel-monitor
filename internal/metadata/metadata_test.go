package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", name))
	if errRead != nil {
		t.Fatalf("read fixture %s: %v", name, errRead)
	}
	return raw
}

func firstLine(raw []byte) []byte {
	return []byte(strings.SplitN(string(raw), "\n", 2)[0])
}

func TestExtractChannelFromStreamFrame(t *testing.T) {
	frame := firstLine(readFixture(t, "sse_channel_frames.jsonl"))
	meta := ExtractChannelFromBody(frame, false)
	if meta == nil {
		t.Fatal("expected channel metadata for a stream frame carrying provider_metadata")
	}
	if meta.FinalProvider != "deepseek" {
		t.Errorf("final_provider = %q, want deepseek", meta.FinalProvider)
	}
	if meta.ResolvedProvider != "deepseek" {
		t.Errorf("resolved_provider = %q, want deepseek", meta.ResolvedProvider)
	}
	if meta.CanonicalSlug != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("canonical_slug = %q", meta.CanonicalSlug)
	}
	if meta.ModelAttemptCount != 1 || meta.TotalProviderAttemptCount != 1 {
		t.Errorf("attempt counts = %d/%d, want 1/1", meta.ModelAttemptCount, meta.TotalProviderAttemptCount)
	}
	if meta.FallbacksAvailableCount != 15 {
		t.Errorf("fallbacks_available_count = %d, want 15", meta.FallbacksAvailableCount)
	}
	if meta.Cost != "0.0000447" || meta.InputCost != "0.0000099" || meta.OutputCost != "0.0000348" {
		t.Errorf("cost fields = %q/%q/%q", meta.Cost, meta.InputCost, meta.OutputCost)
	}
	if meta.GenerationID == "" {
		t.Error("generation_id must be captured")
	}
	if meta.Provider.PromptCacheMissTokens != 33 {
		t.Errorf("prompt_cache_miss_tokens = %d, want 33", meta.Provider.PromptCacheMissTokens)
	}
	if meta.Provider.SystemFingerprint != "fp_fixture0000000000000000000001" {
		t.Errorf("system_fingerprint = %q", meta.Provider.SystemFingerprint)
	}
	if meta.PlanningReasoningText != "" {
		t.Error("planning reasoning text must be dropped when store_planning_reasoning is false")
	}
	if meta.PlanningReasoningLength == 0 {
		t.Error("planning reasoning length must be recorded when the text is not stored")
	}
	if meta.UpstreamPromptTokens != 33 || meta.UpstreamCompletionTokens != 29 {
		t.Errorf("upstream usage = %d/%d", meta.UpstreamPromptTokens, meta.UpstreamCompletionTokens)
	}
	if meta.UpstreamModel != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("upstream model = %q", meta.UpstreamModel)
	}
}

func TestExtractChannelKeepsPlanningReasoningWhenEnabled(t *testing.T) {
	frame := firstLine(readFixture(t, "sse_channel_frames.jsonl"))
	meta := ExtractChannelFromBody(frame, true)
	if meta == nil {
		t.Fatal("expected channel metadata")
	}
	if !strings.Contains(meta.PlanningReasoningText, "System credentials planned for") {
		t.Errorf("planning reasoning should be stored verbatim, got %q", meta.PlanningReasoningText)
	}
}

func TestExtractChannelFromNonStreamBody(t *testing.T) {
	body := readFixture(t, "chat_nonstream_channel.json")
	meta := ExtractChannelFromBody(body, false)
	if meta == nil {
		t.Fatal("expected channel metadata for a non-streaming chat.completion body")
	}
	if meta.FinalProvider != "deepseek" {
		t.Errorf("final_provider = %q, want deepseek", meta.FinalProvider)
	}
}

func TestExtractChannelIgnoresBodiesWithoutMarker(t *testing.T) {
	cases := map[string][]byte{
		"plain json":  []byte(`{"id":"x","choices":[{"delta":{"content":"hi"}}]}`),
		"done frame":  []byte("data: [DONE]"),
		"empty":       nil,
		"empty frame": []byte("data: "),
	}
	for name, body := range cases {
		if meta := ExtractChannelFromBody(body, false); meta != nil {
			t.Errorf("%s: expected nil, got %+v", name, meta)
		}
	}
}

func TestExtractChannelRejectsMalformedJSON(t *testing.T) {
	body := []byte(`data: {"choices":[{"delta":{"provider_metadata":`)
	if meta := ExtractChannelFromBody(body, false); meta != nil {
		t.Fatalf("expected nil for malformed JSON, got %+v", meta)
	}
}

func TestExtractChannelIgnoresMetadataWithoutRoutingOrProvider(t *testing.T) {
	body := []byte(`{"choices":[{"delta":{"provider_metadata":{"gateway":{"cost":"0.1"}}}}]}`)
	if meta := ExtractChannelFromBody(body, false); meta != nil {
		t.Fatalf("neither gateway.routing nor a serving provider is a channel value, got %+v", meta)
	}
}

func TestExtractChannelFromWrappedNonStreamBody(t *testing.T) {
	body := readFixture(t, "chat_nonstream_wrapped_channel.json")
	meta := ExtractChannelFromBody(body, false)
	if meta == nil {
		t.Fatal("expected channel metadata inside Cline's non-streaming wrapper")
	}
	if meta.FinalProvider != "baseten" {
		t.Errorf("final_provider = %q, want baseten", meta.FinalProvider)
	}
	if meta.ResolvedProvider != "baseten" || meta.CanonicalSlug != "zai/glm-5.3" {
		t.Errorf("resolved_provider/canonical_slug = %q/%q", meta.ResolvedProvider, meta.CanonicalSlug)
	}
	if meta.Cost != "0.000248" || meta.GenerationID == "" {
		t.Errorf("cost/generation_id = %q/%q", meta.Cost, meta.GenerationID)
	}
	if meta.UpstreamModel != "zai/glm-5.3" || meta.UpstreamPromptTokens != 33 {
		t.Errorf("upstream model/usage = %q/%d", meta.UpstreamModel, meta.UpstreamPromptTokens)
	}
}

func TestExtractChannelFallsBackToServingProvider(t *testing.T) {
	body := readFixture(t, "chat_nonstream_wrapped_provider.json")
	meta := ExtractChannelFromBody(body, false)
	if meta == nil {
		t.Fatal("a response without gateway routing must still be labelled by its serving provider")
	}
	if meta.FinalProvider != "Relace" {
		t.Errorf("final_provider = %q, want Relace", meta.FinalProvider)
	}
	if meta.ResolvedProvider != "" || meta.Cost != "" {
		t.Errorf("routing-only fields must stay empty, got %q/%q", meta.ResolvedProvider, meta.Cost)
	}
	if meta.UpstreamModel != "z-ai/glm-5.3-flash" || meta.UpstreamCompletionTokens != 45 {
		t.Errorf("upstream model/usage = %q/%d", meta.UpstreamModel, meta.UpstreamCompletionTokens)
	}
}

func TestExtractChannelFromProviderFrame(t *testing.T) {
	frames := strings.Split(strings.TrimSpace(string(readFixture(t, "sse_provider_frames.jsonl"))), "\n")
	meta := ExtractChannelFromBody([]byte(frames[0]), false)
	if meta == nil || meta.FinalProvider != "Relace" {
		t.Fatalf("an SSE frame carrying a serving provider must produce an observation, got %+v", meta)
	}
	if meta.UpstreamModel != "z-ai/glm-5.3-flash" {
		t.Errorf("upstream model = %q", meta.UpstreamModel)
	}
	if done := ExtractChannelFromBody([]byte(frames[len(frames)-1]), false); done != nil {
		t.Errorf("[DONE] must not produce an observation, got %+v", done)
	}
}

func TestExtractChannelPrefersGatewayRoutingOverProvider(t *testing.T) {
	body := []byte(`{"provider":"alibaba","model":"deepseek/deepseek-v4.1-flash",` +
		`"choices":[{"delta":{"provider_metadata":{"gateway":{"routing":{"finalProvider":"deepseek"}}}}}]}`)
	meta := ExtractChannelFromBody(body, false)
	if meta == nil || meta.FinalProvider != "deepseek" {
		t.Fatalf("finalProvider must win over the serving provider, got %+v", meta)
	}
}

func TestExtractChannelWithoutAnyChannelField(t *testing.T) {
	body := []byte(`{"success":true,"data":{"id":"x","model":"m",` +
		`"choices":[{"message":{"role":"assistant","content":"ok"}}]}}`)
	if meta := ExtractChannelFromBody(body, false); meta != nil {
		t.Errorf("a body with neither field must not produce an observation, got %+v", meta)
	}
}

func TestHasChannelMarkerEarlyExit(t *testing.T) {
	if HasChannelMarker([]byte(`{"choices":[{"delta":{"content":"no channel info"}}]}`)) {
		t.Error("body without either needle must be rejected")
	}
	if !HasChannelMarker([]byte(`{"provider_metadata":{}}`)) {
		t.Error("body with the provider_metadata needle must pass the early-out")
	}
	if !HasChannelMarker([]byte(`{"provider":"Relace","model":"z-ai/glm-5.3-flash"}`)) {
		t.Error("body with the serving-provider needle must pass the early-out")
	}
}

func TestMaskAPIKey(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"short":             "****",
		"sk-FIXTURE0000000100": "sk-1****abcd",
	}
	for input, want := range cases {
		if got := MaskAPIKey(input); got != want {
			t.Errorf("MaskAPIKey(%q) = %q, want %q", input, got, want)
		}
	}
}
