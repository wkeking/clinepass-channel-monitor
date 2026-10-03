package channellog

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The two fixtures are frozen captures of real CPA request logs, sanitized. Everything the
// channel view and the later merge depend on is asserted against them field by field: if the
// extraction drifts, these are the values that stop matching.
const (
	fixturePlain = "plain_success.log"
	// fixtureRetry carries a failed attempt before the response that served the request.
	fixtureRetry = "with_error.log"

	fixtureFinalProvider    = "deepseek"
	fixtureResolvedProvider = "deepseek"
	fixtureCanonicalSlug    = "deepseek/deepseek-v4.1-flash"
	fixturePinnedProvider   = "deepseek"
	fixtureAffinityOutcome  = "confirmed"
	fixtureCost             = 0.00001515

	// The usage numbers the frozen captures carry, read out of their own usage frames. The
	// fallback join matches a record's counters against them, so they are pinned here.
	fixturePromptTokens     = 37
	fixtureCompletionTokens = 16
)

// fixtureLog returns one frozen capture.
// TestNonStreamingMessageBlockIsRead pins the alias the gateway uses for a non-streaming
// upstream answer: the same routing block arrives under choices[].message instead of
// choices[].delta. A reader that only knew `delta` would report an empty channel for those
// requests, which is indistinguishable from a failed request.
func TestNonStreamingMessageBlockIsRead(t *testing.T) {
	frame := `{"choices":[{"message":{"provider_metadata":{"gateway":{"cost":"0.5","routing":{` +
		`"finalProvider":"deepseek","resolvedProvider":"deepseek",` +
		`"canonicalSlug":"deepseek/deepseek-v4.1-flash","modelAttemptCount":1,` +
		`"totalProviderAttemptCount":1}}}}}],"model":"cline-pass/deepseek-v4.1-flash"}`
	log := "=== REQUEST INFO ===\nVersion: v8.0.8\nURL: /v1/chat/completions\nTimestamp: 2026-10-02T22:00:00.000000000+08:00\n\n" +
		"=== API RESPONSE 1 ===\ndata: " + frame + "\ndata: [DONE]\n"
	fact, errParse := parseLog(strings.NewReader(log), "nonstream.log", time.Time{})
	if errParse != nil {
		t.Fatalf("parseLog: %v", errParse)
	}
	if fact.FinalProvider != "deepseek" || fact.ResolvedProvider != "deepseek" {
		t.Errorf("fact = %+v, want the channel from the message block", fact)
	}
	if fact.CanonicalSlug != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("canonical_slug = %q, want the routing slug", fact.CanonicalSlug)
	}
	if fact.GatewayCost != 0.5 {
		t.Errorf("gateway_cost = %v, want 0.5", fact.GatewayCost)
	}
}

func fixtureLog(t *testing.T, name string) []byte {
	t.Helper()
	raw, errRead := os.ReadFile(filepath.Join("testdata", name))
	if errRead != nil {
		t.Fatalf("read fixture %s: %v", name, errRead)
	}
	return raw
}

// parseFixture runs the parser over one frozen capture.
func parseFixture(t *testing.T, name string) Fact {
	t.Helper()
	fact, errParse := parseLog(strings.NewReader(string(fixtureLog(t, name))), name, time.Now())
	if errParse != nil {
		t.Fatalf("parse %s: %v", name, errParse)
	}
	return fact
}

// requireChannel compares every channel field of a fact against the values the fixtures carry.
func requireChannel(t *testing.T, fact Fact, context string) {
	t.Helper()
	if fact.FinalProvider != fixtureFinalProvider {
		t.Errorf("%s: final_provider = %q, want %q", context, fact.FinalProvider, fixtureFinalProvider)
	}
	if fact.ResolvedProvider != fixtureResolvedProvider {
		t.Errorf("%s: resolved_provider = %q, want %q", context, fact.ResolvedProvider, fixtureResolvedProvider)
	}
	if fact.CanonicalSlug != fixtureCanonicalSlug {
		t.Errorf("%s: canonical_slug = %q, want %q", context, fact.CanonicalSlug, fixtureCanonicalSlug)
	}
	if fact.OriginalModelID != fixtureCanonicalSlug {
		t.Errorf("%s: original_model_id = %q, want %q", context, fact.OriginalModelID, fixtureCanonicalSlug)
	}
	if fact.PinnedProvider != fixturePinnedProvider {
		t.Errorf("%s: pinned_provider = %q, want %q", context, fact.PinnedProvider, fixturePinnedProvider)
	}
	if fact.AffinityOutcome != fixtureAffinityOutcome {
		t.Errorf("%s: affinity_outcome = %q, want %q", context, fact.AffinityOutcome, fixtureAffinityOutcome)
	}
	if fact.ModelAttemptCount != 1 {
		t.Errorf("%s: model_attempt_count = %d, want 1", context, fact.ModelAttemptCount)
	}
	if fact.TotalProviderAttemptCount != 1 {
		t.Errorf("%s: total_provider_attempt_count = %d, want 1", context, fact.TotalProviderAttemptCount)
	}
	if fact.FallbacksAvailable == nil {
		t.Errorf("%s: fallbacks_available is nil, want an empty list", context)
	}
	if len(fact.FallbacksAvailable) != 0 {
		t.Errorf("%s: fallbacks_available = %v, want the fixture's empty list", context, fact.FallbacksAvailable)
	}
	if math.Abs(fact.GatewayCost-fixtureCost) > 1e-18 {
		t.Errorf("%s: gateway_cost = %v, want %v", context, fact.GatewayCost, fixtureCost)
	}
	if !fact.HasChannel() {
		t.Errorf("%s: the fact must report a channel", context)
	}
}

// requireUsage compares a fact's token counters against the numbers the fixtures carry. They are
// the only thing the fallback join has left when the client sent no Session_id header, so a
// drifted value here is a record that silently stops matching on the host.
func requireUsage(t *testing.T, fact Fact, context string) {
	t.Helper()
	if fact.PromptTokens != fixturePromptTokens {
		t.Errorf("%s: prompt_tokens = %d, want %d", context, fact.PromptTokens, fixturePromptTokens)
	}
	if fact.CompletionTokens != fixtureCompletionTokens {
		t.Errorf("%s: completion_tokens = %d, want %d", context, fact.CompletionTokens, fixtureCompletionTokens)
	}
}

// TestParsePlainSuccessFixture pins the exact values of the sanitized capture: the request
// identity, the routing block the Responses protocol drops, and the frame count it came from.
func TestParsePlainSuccessFixture(t *testing.T) {
	fact := parseFixture(t, fixturePlain)

	want := time.Date(2026, 10, 2, 22, 18, 45, 585796303, time.FixedZone("", 8*3600))
	if !fact.Time.Equal(want) {
		t.Errorf("time = %s, want %s", fact.Time, want)
	}
	if fact.Path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", fact.Path)
	}
	if fact.Method != "POST" {
		t.Errorf("method = %q, want POST", fact.Method)
	}
	requireChannel(t, fact, fixturePlain)
	requireUsage(t, fact, fixturePlain)
	if fact.Frames != 2 {
		t.Errorf("frames = %d, want 2: the section holds two JSON frames and the [DONE] marker", fact.Frames)
	}
	if fact.AttemptsSeen != 1 {
		t.Errorf("attempts_seen = %d, want 1", fact.AttemptsSeen)
	}
	if fact.HadErrorResponse {
		t.Error("had_error_response = true for a capture with no error section")
	}
	// This client sends no Session_id header, which is exactly the case the token-and-time
	// fallback exists for: the fact has to say the header was absent.
	if fact.HasSession || fact.SessionID != "" || fact.SessionUUID != "" {
		t.Errorf("session = (%q, %q, %v), want an absent header recorded as such",
			fact.SessionID, fact.SessionUUID, fact.HasSession)
	}
	if fact.SourceFile != fixturePlain {
		t.Errorf("source_file = %q, want %q", fact.SourceFile, fixturePlain)
	}
}

// TestParseRetryFixtureUsesTheServedChannel covers the retry shape: a failed attempt is written
// first, the successful response follows, and the fact must describe the response that actually
// served the request while still recording that an attempt failed.
func TestParseRetryFixtureUsesTheServedChannel(t *testing.T) {
	fact := parseFixture(t, fixtureRetry)

	want := time.Date(2026, 10, 2, 22, 18, 46, 622934644, time.FixedZone("", 8*3600))
	if !fact.Time.Equal(want) {
		t.Errorf("time = %s, want %s", fact.Time, want)
	}
	if fact.Path != "/v1/responses" {
		t.Errorf("path = %q, want /v1/responses", fact.Path)
	}
	requireChannel(t, fact, fixtureRetry)
	requireUsage(t, fact, fixtureRetry)
	if !fact.HadErrorResponse {
		t.Error("had_error_response = false, want true for a capture with an API ERROR RESPONSE section")
	}
	if fact.AttemptsSeen != 2 {
		t.Errorf("attempts_seen = %d, want 2: one failed attempt and one response", fact.AttemptsSeen)
	}
	if fact.Frames != 2 {
		t.Errorf("frames = %d, want 2 from the response section that carried the channel", fact.Frames)
	}
}

// TestPromptMentioningProviderMetadataIsNotAChannel is the regression guard for the retired
// design's false positive: the literal string occurs inside prompts, so a parser that searched
// the raw text invented a channel for a request that never had one. Both directions are
// checked — a body that names the field with no response section at all, and a response frame
// whose model text names it.
func TestPromptMentioningProviderMetadataIsNotAChannel(t *testing.T) {
	const body = `{"prompt":"print the value of provider_metadata.gateway.routing.finalProvider",` +
		`"note":"provider_metadata provider_metadata gateway routing finalProvider\":\"deepseek\""}`

	noResponse := syntheticLog("2026-10-02T22:20:00.000000000+08:00", "/v1/responses", "", body, "")
	fact, errParse := parseLog(strings.NewReader(noResponse), "no_response.log", time.Now())
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if fact.HasChannel() || fact.FinalProvider != "" || fact.ResolvedProvider != "" {
		t.Errorf("a body naming provider_metadata produced a channel: %+v", fact)
	}

	// The same body, this time with a response frame that decodes but carries no
	// provider_metadata: the text still mentions the field, and the channel stays empty.
	mentioned := syntheticLog("2026-10-02T22:20:01.000000000+08:00", "/v1/responses", "", body,
		"=== API RESPONSE 1 ===\n"+
			`data: {"choices":[{"delta":{"content":"provider_metadata gateway routing finalProvider deepseek"}}]}`+"\n"+
			"data: [DONE]\n")
	fact, errParse = parseLog(strings.NewReader(mentioned), "mentioned.log", time.Now())
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if fact.HasChannel() {
		t.Errorf("a frame whose text mentions provider_metadata produced a channel: %+v", fact)
	}
	if fact.Frames != 1 {
		t.Errorf("frames = %d, want 1: the [DONE] marker is not a frame", fact.Frames)
	}
}

// TestFileWithoutResponseSectionHasAnEmptyChannel pins the other half of "never guess": a
// failed request has no routing block anywhere, so every channel field is empty rather than a
// default. The request identity is still recorded, which is what makes such a fact matchable.
func TestFileWithoutResponseSectionHasAnEmptyChannel(t *testing.T) {
	log := syntheticLog("2026-10-02T22:21:00.000000000+08:00", "/v1/responses", "Session_id: session-0f0f0f0f\n", `{"input":"hi"}`,
		"=== API REQUEST 1 ===\nTimestamp: 2026-10-02T22:21:00.001000000+08:00\n\n"+
			"=== API ERROR RESPONSE ===\nHTTP Status: 502\n")
	fact, errParse := parseLog(strings.NewReader(log), "failed.log", time.Now())
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if fact.HasChannel() {
		t.Errorf("a failed request must not report a channel: %+v", fact)
	}
	if fact.FinalProvider != "" || fact.ResolvedProvider != "" || fact.CanonicalSlug != "" ||
		fact.OriginalModelID != "" || fact.PinnedProvider != "" {
		t.Errorf("every channel field must stay empty, got %+v", fact)
	}
	if fact.Frames != 0 {
		t.Errorf("frames = %d, want 0", fact.Frames)
	}
	if !fact.HadErrorResponse || fact.AttemptsSeen != 1 {
		t.Errorf("had_error_response = %v with attempts_seen = %d, want true and 1",
			fact.HadErrorResponse, fact.AttemptsSeen)
	}
	if !fact.HasSession || fact.SessionID != "session-0f0f0f0f" || fact.SessionUUID != "0f0f0f0f" {
		t.Errorf("session = (%q, %q, %v), want the header read and the prefix stripped",
			fact.SessionID, fact.SessionUUID, fact.HasSession)
	}
	// A request that never got an answer names no usage either: both counters stay zero.
	if fact.PromptTokens != 0 || fact.CompletionTokens != 0 {
		t.Errorf("usage = (%d, %d), want 0/0 without a response section",
			fact.PromptTokens, fact.CompletionTokens)
	}
}

// TestLastParseableResponseSectionWins pins "take the last API RESPONSE that parses": when a
// later section carries no routing block, the earlier one's channel is still the answer, and a
// later section that does carry one replaces it.
func TestLastParseableResponseSectionWins(t *testing.T) {
	log := syntheticLog("2026-10-02T22:22:00.000000000+08:00", "/v1/responses", "", `{"input":"hi"}`,
		"=== API RESPONSE 1 ===\n"+channelFrame("deepseek", "deepseek", "deepseek/deepseek-v4.1-flash", 1)+"\n\n"+
			"=== API RESPONSE 2 ===\n"+channelFrame("other", "other", "vendor/other-model", 2)+"\n")
	fact, errParse := parseLog(strings.NewReader(log), "two.log", time.Now())
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if fact.FinalProvider != "other" || fact.CanonicalSlug != "vendor/other-model" || fact.ModelAttemptCount != 2 {
		t.Errorf("the last parseable section must win, got %+v", fact)
	}
	if fact.AttemptsSeen != 2 {
		t.Errorf("attempts_seen = %d, want 2", fact.AttemptsSeen)
	}
}

// TestUsageNumbersComeFromTheWinningSection pins where the counters are read from. The
// upstream reports usage more than once — and a section that carries usage without a routing
// block is an attempt the request was not served by — so the numbers have to describe the same
// answer the channel does. Pairing a record with another attempt's counters would be the same
// class of guess as inventing a channel.
func TestUsageNumbersComeFromTheWinningSection(t *testing.T) {
	log := syntheticLog("2026-10-02T22:24:00.000000000+08:00", "/v1/responses", "", `{"input":"hi"}`,
		"=== API RESPONSE 1 ===\n"+
			channelFrame("deepseek", "deepseek", "deepseek/deepseek-v4.1-flash", 1)+"\n"+
			usageFrame(37, 4)+"\n"+
			usageFrame(37, 16)+"\n\n"+
			"=== API RESPONSE 2 ===\n"+
			usageFrame(99, 99)+"\n")
	fact, errParse := parseLog(strings.NewReader(log), "usage_sections.log", time.Now())
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	// The last usage frame of the winning section wins over the earlier one in it, and the
	// later section's numbers are not the served answer's.
	if fact.PromptTokens != 37 || fact.CompletionTokens != 16 {
		t.Errorf("usage = (%d, %d), want the winning section's last frame (37, 16)",
			fact.PromptTokens, fact.CompletionTokens)
	}
	if fact.FinalProvider != "deepseek" {
		t.Errorf("final_provider = %q, want the channel of the section the usage came from", fact.FinalProvider)
	}
}

// TestSectionWithoutUsageYieldsZeroCounters is the other half of "never a guess": a section
// whose frames name no usage leaves both counters at zero, and so does a file with no response
// section at all.
func TestSectionWithoutUsageYieldsZeroCounters(t *testing.T) {
	log := syntheticLog("2026-10-02T22:25:00.000000000+08:00", "/v1/responses", "", `{"input":"hi"}`,
		"=== API RESPONSE 1 ===\n"+channelFrame("deepseek", "deepseek", "deepseek/deepseek-v4.1-flash", 1)+"\n")
	fact, errParse := parseLog(strings.NewReader(log), "no_usage.log", time.Now())
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if !fact.HasChannel() {
		t.Fatal("the frame carries a routing block, so the fact must report a channel")
	}
	if fact.PromptTokens != 0 || fact.CompletionTokens != 0 {
		t.Errorf("usage = (%d, %d), want 0/0 for a section that named none",
			fact.PromptTokens, fact.CompletionTokens)
	}
}

// TestLongLineIsSkippedWithoutLosingSections proves the reader resynchronises after a line it
// refuses to hold: the request body of a long context is one line, and the response section
// after it must still be parsed.
func TestLongLineIsSkippedWithoutLosingSections(t *testing.T) {
	body := strings.Repeat("x", 3*readBuffer)
	log := syntheticLog("2026-10-02T22:23:00.000000000+08:00", "/v1/responses", "", body,
		"=== API RESPONSE 1 ===\n"+channelFrame("deepseek", "deepseek", "deepseek/deepseek-v4.1-flash", 1)+"\n")
	fact, errParse := parseLog(strings.NewReader(log), "long.log", time.Now())
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	if fact.FinalProvider != "deepseek" {
		t.Errorf("final_provider = %q, want deepseek: the section after the long line must still parse", fact.FinalProvider)
	}
}

// syntheticLog renders a request log in CPA's section order. tail is everything from the first
// API section on, so a test can build a retry, an error-only file, or nothing at all.
func syntheticLog(timestamp, url, headers, body, tail string) string {
	var builder strings.Builder
	builder.WriteString("=== REQUEST INFO ===\n")
	builder.WriteString("Version: v8.0.8\n")
	builder.WriteString("URL: " + url + "\n")
	builder.WriteString("Method: POST\n")
	builder.WriteString("Downstream Transport: http\n")
	builder.WriteString("Upstream Transport: http\n")
	builder.WriteString("Timestamp: " + timestamp + "\n\n")
	builder.WriteString("=== HEADERS ===\n")
	builder.WriteString("Authorization: Bearer sk-REDACTED-fixture-only\n")
	if headers != "" {
		builder.WriteString(headers)
	}
	builder.WriteString("\n=== REQUEST BODY ===\n")
	builder.WriteString(body)
	builder.WriteString("\n\n")
	builder.WriteString(tail)
	builder.WriteString("=== RESPONSE ===\nStatus: 200\n")
	return builder.String()
}

// channelFrame renders one SSE frame carrying a gateway routing block, the way the upstream
// response does.
func channelFrame(finalProvider, resolvedProvider, slug string, attempts int) string {
	count := strconv.Itoa(attempts)
	return `data: {"choices":[{"delta":{"provider_metadata":{"gateway":{"cost":"0.00001515","routing":{` +
		`"finalProvider":"` + finalProvider + `",` +
		`"resolvedProvider":"` + resolvedProvider + `",` +
		`"canonicalSlug":"` + slug + `",` +
		`"originalModelId":"` + slug + `",` +
		`"modelAttemptCount":` + count + `,` +
		`"totalProviderAttemptCount":` + count + `,` +
		`"fallbacksAvailable":[],` +
		`"affinity":{"outcome":"confirmed","pinnedProvider":"` + finalProvider + `"}}}}}}],"id":"gen_test"}`
}

// usageFrame renders one SSE frame carrying the upstream's token counters, the way the last
// frame of a streamed answer does. It carries no routing block: the upstream reports usage
// cumulatively, and the counters must still be read from the section that served the request.
func usageFrame(prompt, completion int) string {
	return `data: {"choices":[],"usage":{"prompt_tokens":` + strconv.Itoa(prompt) +
		`,"completion_tokens":` + strconv.Itoa(completion) + `}}`
}
