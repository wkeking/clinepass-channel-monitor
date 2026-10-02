package observation

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixtures below mirror the payload the production host actually hands to usage.handle,
// captured on the live host by a throwaway probe plugin. The field names are the SDK's Go
// field names, and Latency/TTFT are integer nanoseconds because the SDK sends time.Duration
// values; the payload is protocol independent, which is why the collector no longer reads the
// response stream at all.

const (
	// providerFixture is the CPA credential the fixture request was served with. It is what the
	// record stores as cpa_provider, and it is NOT a gateway channel.
	providerFixture = "openai-compatible-cline1"
	// providerBaseline is the channel the recorder is configured to treat as the official one.
	providerBaseline = "deepseek"
	// providerOther is the credential of a second request, and the baseline of the tests that
	// flip the question around.
	providerOther = "alibaba"
)

// fixtureNow is the recorder clock every fixture timestamp is placed after. It is fixed so
// the assertions are numbers instead of a race with the wall clock.
var fixtureNow = time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)

// liveUsagePayload is the payload shape captured on the host, verbatim apart from the
// credential hashes. It pins the wire contract: a renamed field in usageWire would show up
// here as a missing value rather than silently collecting nothing on the next release.
const liveUsagePayload = `{"Provider":"openai-compatible-cline1","BaseURL":"https://api.cline.bot","ExecutorType":"OpenAICompatExecutor","Model":"cline-pass/deepseek-v4.1-flash","Alias":"deepseek-flash-2","APIKey":"sk_credential-fixture","RequestID":"01a0fb77-fixture","TraceID":"01a0fb77-fixture","SessionID":"codex:sess-fixture-1","ParentSessionID":"","AuthID":"openai-compatibility:cline1:7cb99c3ced51","AuthIndex":"b6c17a2d5d0c3862","AuthType":"apikey","Source":"sk_credential-fixture","ReasoningEffort":"high","ServiceTier":"auto","ResponseServiceTier":"","ResponseModel":"deepseek/deepseek-v4.1-flash","Generate":true,"Stream":true,"Failed":false,"Failure":{"StatusCode":0,"Body":""},"Detail":{"InputTokens":37,"OutputTokens":16,"ReasoningTokens":14,"CachedTokens":0,"CacheReadTokens":0,"CacheCreationTokens":0,"TotalTokens":53},"RequestedAt":"2026-10-02T15:14:23Z","Latency":1097125788,"TTFT":643260112}`

// testClock makes the window bounds and the timestamp fallback deterministic.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// newRecorder builds a recorder with no fact source: every record it stores is unjoined, which
// is the deployment with the request-log scanner switched off.
func newRecorder(t *testing.T) (*Recorder, *testClock) {
	t.Helper()
	return newRecorderWithBaseline(t, providerBaseline)
}

func newRecorderWithBaseline(t *testing.T, baseline string) (*Recorder, *testClock) {
	t.Helper()
	return newRecorderWithFacts(t, baseline, nil)
}

// usageFixture is one payload in the shape the host sends, with every field these tests read
// filled in. Provider and RequestedAt are the two a test always decides for itself.
func usageFixture(provider string, requestedAt time.Time) usageWire {
	stamp := requestedAt.UTC().Format(time.RFC3339)
	return usageWire{
		Provider:        provider,
		BaseURL:         "https://api.cline.bot",
		ExecutorType:    "OpenAICompatExecutor",
		Model:           "cline-pass/deepseek-v4.1-flash",
		Alias:           "deepseek-flash-2",
		APIKey:          "sk_credential-fixture",
		RequestID:       "req-fixture-1",
		TraceID:         "req-fixture-1",
		SessionID:       "codex:sess-fixture-1",
		AuthID:          "openai-compatibility:cline1:7cb99c3ced51",
		AuthIndex:       "b6c17a2d5d0c3862",
		AuthType:        "apikey",
		Source:          "sk_credential-fixture",
		ReasoningEffort: "high",
		ServiceTier:     "auto",
		ResponseModel:   "deepseek/deepseek-v4.1-flash",
		Generate:        true,
		Stream:          true,
		Detail:          usageDetail{InputTokens: 37, OutputTokens: 16, ReasoningTokens: 14, TotalTokens: 53},
		RequestedAt:     stamp,
		Latency:         1097125788,
		TTFT:            643260112,
	}
}

// feedUsage hands one record to the collector the way the host does, then waits for it to
// reach the file: the writer is asynchronous and the page reads the same file this reads.
func feedUsage(t *testing.T, recorder *Recorder, wire usageWire) {
	t.Helper()
	recorder.ObserveUsage(&wire)
	recorder.Flush()
}

func recordsOf(t *testing.T, recorder *Recorder) []Record {
	t.Helper()
	recorder.Flush()
	records, errLoad := recorder.RecordsSince(mustWindow(t, "24h"))
	if errLoad != nil {
		t.Fatalf("RecordsSince: %v", errLoad)
	}
	return records
}

// useRecorder installs a recorder as the usage entry point's target.
func useRecorder(t *testing.T, recorder *Recorder) {
	t.Helper()
	SetActive(recorder)
	t.Cleanup(func() { SetActive(nil) })
}

// decodeKeepAnswer asserts the answer is the "no change" envelope the host expects.
func decodeKeepAnswer(t *testing.T, answer []byte, context string) {
	t.Helper()
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(answer, &envelope); errUnmarshal != nil {
		t.Fatalf("%s: answer is not the envelope the host expects: %v (%s)", context, errUnmarshal, answer)
	}
	if !envelope.OK {
		t.Fatalf("%s: answer ok=false: %s", context, answer)
	}
	var result map[string]any
	if errUnmarshal := json.Unmarshal(envelope.Result, &result); errUnmarshal != nil {
		t.Fatalf("%s: result is not an object: %v (%s)", context, errUnmarshal, envelope.Result)
	}
	if len(result) != 0 {
		t.Fatalf("%s: the plugin answered with a change: %s", context, envelope.Result)
	}
}

// TestHandleUsageStoresTheMappedRecord drives the captured payload through the ABI entry
// point and checks the stored record field by field: this is the whole point of the release,
// because a field that stopped being mapped is exactly how the feature went blind before.
func TestHandleUsageStoresTheMappedRecord(t *testing.T) {
	recorder, _ := newRecorder(t)
	useRecorder(t, recorder)

	answer, errHandle := HandleUsage([]byte(liveUsagePayload))
	if errHandle != nil {
		t.Fatalf("HandleUsage returned an error: %v", errHandle)
	}
	decodeKeepAnswer(t, answer, "HandleUsage")

	records := recordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want exactly 1 stored record, got %d: %+v", len(records), records)
	}
	record := records[0]
	for _, check := range []struct {
		name string
		got  any
		want any
	}{
		{"v", record.Schema, schemaVersion},
		{"time", record.Time.UTC().Format(time.RFC3339), "2026-10-02T15:14:23Z"},
		{"request_id", record.RequestID, "01a0fb77-fixture"},
		{"session_id", record.SessionID, "codex:sess-fixture-1"},
		{"model", record.Model, "cline-pass/deepseek-v4.1-flash"},
		{"alias", record.Alias, "deepseek-flash-2"},
		{"upstream_model", record.UpstreamModel, "deepseek/deepseek-v4.1-flash"},
		{"canonical_slug", record.CanonicalSlug, "deepseek/deepseek-v4.1-flash"},
		{"cpa_provider", record.CPProvider, providerFixture},
		{"auth_id", record.AuthID, "openai-compatibility:cline1:7cb99c3ced51"},
		{"auth_index", record.AuthIndex, "b6c17a2d5d0c3862"},
		{"auth_type", record.AuthType, "apikey"},
		{"executor_type", record.ExecutorType, "OpenAICompatExecutor"},
		{"reasoning_effort", record.ReasoningEffort, "high"},
		{"service_tier", record.ServiceTier, "auto"},
		{"stream", record.Stream, true},
		{"failed", record.Failed, false},
		{"status_code", record.StatusCode, 200},
		{"ttft_ms", record.TTFTMs, int64(643)},
		{"duration_ms", record.DurationMs, int64(1097)},
		{"decode_ms", record.DecodeMs, int64(454)},
		{"input_tokens", record.InputTokens, int64(37)},
		{"output_tokens", record.OutputTokens, int64(16)},
		{"reasoning_tokens", record.ReasoningTokens, int64(14)},
		{"cached_tokens", record.CachedTokens, int64(0)},
		{"cache_read_tokens", record.CacheReadTokens, int64(0)},
		{"cache_creation_tokens", record.CacheCreationTokens, int64(0)},
		{"total_tokens", record.TotalTokens, int64(53)},
	} {
		if check.got != check.want {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
	if want := 16.0 / 0.454; record.TPS < want-0.01 || record.TPS > want+0.01 {
		t.Errorf("tokens_per_second = %v, want %v", record.TPS, want)
	}

	// The keys the usage payload cannot fill must be absent, not zero-filled: a v3 record that
	// carried "frames":0 would make an old reader believe a stream had no chunk, and one that
	// carried the gateway keys would make a reader believe a channel had been measured.
	raw := storedLine(t, recorder, record.RequestID)
	for _, gone := range []string{"frames", "cost_usd", "is_byok", "protocol", "source_format", "user_agent", "claude_code_version", "client_app", "generation_id", "affinity", "fallbacks_available", "model_attempt_count", "total_provider_attempt_count", "upstream_request_id", "final_provider", "resolved_provider", "gateway_provider", "gateway_resolved_provider", "gateway_slug", "gateway_attempts", "gateway_cost", "channel_source"} {
		if strings.Contains(raw, `"`+gone+`"`) {
			t.Errorf("a v3 record carries the key %q: %s", gone, raw)
		}
	}
	// The payload carries credential hashes (APIKey, Source); the record must never persist
	// credential material.
	if strings.Contains(raw, "sk_credential-fixture") {
		t.Errorf("the stored record carries credential material: %s", raw)
	}

	health := recorder.Health()
	if health.Events != 1 || health.FailedEvents != 0 || health.DecodeFailures != 0 || health.Written != 1 || health.Dropped != 0 {
		t.Errorf("health = %+v, want 1 event, 0 failed, 0 decode failures, 1 written, 0 dropped", health)
	}
	if health.LastRecordAt == nil || health.LastWriteAt == nil {
		t.Errorf("health timestamps not set: %+v", health)
	}
}

// TestHandleUsageStoresAFailedRecord pins the failure path: the host's own status and
// verdict are kept, and the failure counter moves.
func TestHandleUsageStoresAFailedRecord(t *testing.T) {
	recorder, _ := newRecorder(t)
	useRecorder(t, recorder)

	wire := usageFixture(providerOther, fixtureNow.Add(-time.Minute))
	wire.Failed = true
	wire.Failure = usageFailure{StatusCode: 503, Body: "upstream unavailable"}
	wire.Detail.OutputTokens = 0
	raw, errMarshal := json.Marshal(wire)
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	if _, errHandle := HandleUsage(raw); errHandle != nil {
		t.Fatalf("HandleUsage returned an error: %v", errHandle)
	}

	records := recordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want exactly 1 stored record, got %d: %+v", len(records), records)
	}
	record := records[0]
	if !record.Failed {
		t.Error("failed = false, want true for a payload the host reported as failed")
	}
	if record.StatusCode != 503 {
		t.Errorf("status_code = %d, want the host's own 503", record.StatusCode)
	}
	if record.CPProvider != providerOther {
		t.Errorf("cpa_provider = %q, want %q", record.CPProvider, providerOther)
	}
	if record.FinalProvider != "" || record.ChannelSource != "" {
		t.Errorf("record = %+v, want no gateway channel: this record has no fact, and a failure never gets one", record)
	}
	health := recorder.Health()
	if health.Events != 1 || health.FailedEvents != 1 {
		t.Errorf("health = %+v, want 1 event and 1 failed event", health)
	}
	// The window view carries the failure itself: the process counters above reset with the
	// plugin, so a page that reported them as the window's failures would read 0 after any
	// restart while the records still hold hundreds of 502s.
	summary := recorder.Summary(mustWindow(t, "24h"))
	if summary.FailedRequests != 1 {
		t.Errorf("summary.failed_requests = %d, want 1", summary.FailedRequests)
	}
	// The failure is countable but not attributable: it has no channel block, so it is not in
	// a channel row — it is one of the unresolved requests, and it must never be recorded as
	// the baseline channel.
	if summary.Resolved != 0 || summary.Unresolved != 1 {
		t.Errorf("resolved/unresolved = %d/%d, want 0/1 for a record with no fact",
			summary.Resolved, summary.Unresolved)
	}
	if len(summary.Providers) != 0 {
		t.Errorf("providers = %+v, want no channel row for a failure", summary.Providers)
	}
	if summary.OffBaseline != 0 {
		t.Errorf("off_baseline = %d, want 0: an unknown channel is not an off-baseline request", summary.OffBaseline)
	}
	// The credential dimension still counts it: the host reported which key carried the
	// failure, which is exactly how a bad key is found.
	credentials := map[string]int64{}
	for _, row := range summary.CPAProviders {
		credentials[row.Provider] = row.Failed
	}
	if credentials[providerOther] != 1 {
		t.Errorf("cpa_providers failed counts = %+v, want %s:1", credentials, providerOther)
	}
	hourFailed := int64(0)
	for _, point := range summary.Hours {
		hourFailed += point.Failed
	}
	if hourFailed != 1 {
		t.Errorf("hour failed total = %d, want 1", hourFailed)
	}
}

// TestObserveUsageLeavesSpeedUnmeasuredForAShortWindow keeps the TPS floor: a short decode
// window measures the batching, not the speed, so the derived speed is dropped while the raw
// numbers stay.
func TestObserveUsageLeavesSpeedUnmeasuredForAShortWindow(t *testing.T) {
	recorder, _ := newRecorder(t)
	wire := usageFixture(providerBaseline, fixtureNow.Add(-time.Minute))
	wire.Latency = 54 * time.Millisecond
	wire.TTFT = 50 * time.Millisecond
	feedUsage(t, recorder, wire)

	records := recordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want exactly 1 record, got %d: %+v", len(records), records)
	}
	record := records[0]
	if record.DecodeMs <= 0 || record.DecodeMs >= minDecodeWindowMs {
		t.Fatalf("decode window = %d ms, want 0 < decode < %d", record.DecodeMs, minDecodeWindowMs)
	}
	if record.TPS != 0 {
		t.Errorf("tokens_per_second = %v, want 0: a %d ms window measures the batching, not the speed",
			record.TPS, record.DecodeMs)
	}
	if record.OutputTokens == 0 || record.TTFTMs == 0 || record.DurationMs == 0 {
		t.Errorf("record lost its raw numbers: %+v", record)
	}
	if summary := recorder.Summary(mustWindow(t, "24h")); summary.DecodeP50TPS != 0 {
		t.Errorf("window decode_p50_tps = %v, want 0: an unmeasured speed must not reach the page",
			summary.DecodeP50TPS)
	}
}

// TestObserveUsageFallsBackOnMissingFields covers the three fallbacks the contract asks for:
// the trace id when there is no request id, the recorder clock when the timestamp does not
// parse, and input+output when the host reports no total.
func TestObserveUsageFallsBackOnMissingFields(t *testing.T) {
	recorder, clock := newRecorder(t)
	wire := usageFixture(providerBaseline, fixtureNow)
	wire.RequestID = ""
	wire.TraceID = "trace-fixture"
	wire.RequestedAt = "not a timestamp"
	wire.Detail.TotalTokens = 0
	feedUsage(t, recorder, wire)

	records := recordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want exactly 1 record, got %d: %+v", len(records), records)
	}
	record := records[0]
	if record.RequestID != "trace-fixture" {
		t.Errorf("request_id = %q, want the trace id", record.RequestID)
	}
	if !record.Time.Equal(clock.at) {
		t.Errorf("time = %v, want the recorder clock %v", record.Time, clock.at)
	}
	if want := record.InputTokens + record.OutputTokens; record.TotalTokens != want {
		t.Errorf("total_tokens = %d, want input+output = %d", record.TotalTokens, want)
	}
}

// TestStoreRoundTripAndHourlySummary keeps the store and window tests: what was fed comes back
// through RecordsSince, and the window adds up per credential even when there is no channel
// source at all. The channel dimension is covered where there are facts to join; here nothing
// is joined, which is the deployment with the request-log scanner switched off.
func TestStoreRoundTripAndHourlySummary(t *testing.T) {
	recorder, _ := newRecorder(t)
	first := usageFixture(providerBaseline, fixtureNow.Add(-90*time.Minute))
	first.RequestID = "req-on-baseline"
	second := usageFixture(providerOther, fixtureNow.Add(-10*time.Minute))
	second.RequestID = "req-off-baseline"
	feedUsage(t, recorder, first)
	feedUsage(t, recorder, second)

	records := recordsOf(t, recorder)
	if len(records) != 2 {
		t.Fatalf("want 2 records back from the store, got %d: %+v", len(records), records)
	}
	if records[0].RequestID != "req-off-baseline" || records[1].RequestID != "req-on-baseline" {
		t.Errorf("records are not newest first: %q then %q", records[0].RequestID, records[1].RequestID)
	}
	if records[0].SessionID != "codex:sess-fixture-1" || records[0].InputTokens != 37 || records[0].DurationMs != 1097 {
		t.Errorf("the record did not survive the JSONL round trip: %+v", records[0])
	}

	summary := recorder.Summary(mustWindow(t, "24h"))
	if summary.Resolved != 0 || summary.Unresolved != 2 {
		t.Errorf("resolved/unresolved = %d/%d, want 0/2: without a channel source nothing joins",
			summary.Resolved, summary.Unresolved)
	}
	if summary.OffBaseline != 0 || summary.OffRatio != 0 {
		t.Errorf("off_baseline = %d (ratio %v), want 0: the question is asked of the real channel only",
			summary.OffBaseline, summary.OffRatio)
	}
	if summary.Baseline != providerBaseline {
		t.Errorf("baseline_provider = %q, want %q", summary.Baseline, providerBaseline)
	}
	if summary.Channels != 0 || len(summary.Providers) != 0 {
		t.Errorf("channels = %d with rows %+v, want no channel dimension", summary.Channels, summary.Providers)
	}
	seen := map[string]ProviderStat{}
	for _, provider := range summary.CPAProviders {
		seen[provider.Provider] = provider
	}
	if len(seen) != 2 {
		t.Fatalf("cpa_providers = %+v, want one row per credential", summary.CPAProviders)
	}
	if got := seen[providerBaseline]; got.Requests != 1 || got.Ratio != 0.5 {
		t.Errorf("%s credential row = %+v, want 1 request at ratio 0.5", providerBaseline, got)
	}
	if got := seen[providerOther]; got.Requests != 1 || got.Ratio != 0.5 {
		t.Errorf("%s credential row = %+v, want 1 request at ratio 0.5", providerOther, got)
	}
	// The timeline covers the window hour by hour, not only the hours that saw traffic.
	if len(summary.Hours) != 25 {
		t.Errorf("hours = %d, want one point per hour mark of the window", len(summary.Hours))
	}
	firstHour, lastHour := summary.Hours[0], summary.Hours[len(summary.Hours)-1]
	if !firstHour.Hour.Equal(summary.From.Truncate(time.Hour)) || !lastHour.Hour.Equal(summary.To.Truncate(time.Hour)) {
		t.Errorf("timeline runs %v..%v, want %v..%v", firstHour.Hour, lastHour.Hour,
			summary.From.Truncate(time.Hour), summary.To.Truncate(time.Hour))
	}
	unresolved := int64(0)
	for _, point := range summary.Hours {
		unresolved += point.Unresolved
	}
	if unresolved != 2 {
		t.Errorf("the timeline's unresolved total = %d, want both records: every hour has to say how much of it is unknown", unresolved)
	}
}

// TestWriteCSVCarriesTheNewColumns keeps the export useful: the credential column is
// cpa_provider, the real channel has its own columns, off_baseline is decided on the channel,
// and the columns that only ever held a credential under a channel's name are gone.
func TestWriteCSVCarriesTheNewColumns(t *testing.T) {
	source := &factSet{}
	recorder, clock := newRecorderWithFacts(t, providerBaseline, source)
	at := clock.at.Add(-time.Minute)
	wire := usageFixture(providerFixture, at)
	wire.SessionID = joinRecordSession
	wire.Detail.CacheReadTokens = 512
	wire.Detail.CacheCreationTokens = 64
	stampUsage(&wire, at)
	feedUsage(t, recorder, wire)
	source.set(gatewayFact(at.Add(joinArrivalGap), joinSessionUUID, joinGatewayOther))

	lines := csvLines(t, recorder)
	header := strings.Split(lines[0], ",")
	row := strings.Split(lines[1], ",")
	if len(header) != len(row) {
		t.Fatalf("header has %d columns, row has %d", len(header), len(row))
	}
	column := map[string]string{}
	for index, name := range header {
		column[name] = row[index]
	}
	for _, name := range []string{
		"cpa_provider", "gateway_provider", "gateway_resolved_provider", "gateway_slug",
		"gateway_attempts", "gateway_cost", "channel_source", "pinned_provider", "canonical_slug",
		"off_baseline", "auth_id", "auth_index", "auth_type", "alias", "failed", "cache_read_tokens",
		"cache_creation_tokens", "executor_type", "reasoning_effort", "service_tier",
	} {
		if _, ok := column[name]; !ok {
			t.Fatalf("the export has no %s column: %v", name, header)
		}
	}
	for _, gone := range []string{
		"final_provider", "resolved_provider", "frames", "cost_usd", "is_byok", "affinity",
		"fallbacks_available", "model_attempts", "provider_attempts", "user_agent", "claude_code_version",
	} {
		if _, ok := column[gone]; ok {
			t.Errorf("the export still carries the retired column %s", gone)
		}
	}
	for name, want := range map[string]string{
		"cpa_provider":              providerFixture,
		"gateway_provider":          joinGatewayOther,
		"gateway_resolved_provider": joinGatewayOther,
		"gateway_slug":              joinGatewayOther + "/deepseek-v4.1-flash",
		"gateway_attempts":          "1",
		"gateway_cost":              "0.00001515",
		"channel_source":            ChannelSourceLog,
		"alias":                     "deepseek-flash-2",
		"auth_type":                 "apikey",
		"executor_type":             "OpenAICompatExecutor",
		"reasoning_effort":          "high",
		"service_tier":              "auto",
		"failed":                    "false",
		"cache_read_tokens":         "512",
		"cache_creation_tokens":     "64",
		"status_code":               "200",
		"off_baseline":              "yes",
		"tokens_per_second":         "35.24",
	} {
		if column[name] != want {
			t.Errorf("csv %s = %q, want %q", name, column[name], want)
		}
	}

	// The same row against the other baseline: the column is computed when it is written, so
	// the answer follows the question. The comparison is case-insensitive.
	recorder.options.Baseline = strings.ToUpper(joinGatewayOther)
	if got := csvColumn(t, recorder, "off_baseline"); got != "" {
		t.Errorf("off_baseline = %q for a row on the flipped baseline, want it empty", got)
	}
	// The credential column is not the off-baseline column: it keeps its value either way.
	if got := csvColumn(t, recorder, "cpa_provider"); got != providerFixture {
		t.Errorf("cpa_provider = %q, want the credential %q", got, providerFixture)
	}
}

// TestWriteCSVLeavesTheChannelEmptyWithoutAFact keeps the export from answering a question it
// has no data for: a record with no fact exports its credential and empty channel columns, and
// the two numeric channel columns stay at zero rather than being filled with a guess.
func TestWriteCSVLeavesTheChannelEmptyWithoutAFact(t *testing.T) {
	recorder, clock := newRecorder(t)
	at := clock.at.Add(-time.Minute)
	wire := usageFixture(providerFixture, at)
	stampUsage(&wire, at)
	feedUsage(t, recorder, wire)

	header, rows := csvExport(t, recorder)
	if len(rows) != 1 {
		t.Fatalf("want 1 exported row, got %d", len(rows))
	}
	columns := csvColumnIndex(header)
	for name, want := range map[string]string{
		"cpa_provider":     providerFixture,
		"gateway_provider": "",
		"gateway_slug":     "",
		"channel_source":   "",
		"off_baseline":     "",
		"gateway_attempts": "0",
		"gateway_cost":     "0.00000000",
	} {
		if got := rows[0][columns[name]]; got != want {
			t.Errorf("csv %s = %q, want %q", name, got, want)
		}
	}
}

// TestWarmupReadsV1Lines keeps the old on-disk schema readable: a line written by the retired
// stream-sniffing source names its channel with keys this build does not fill, and its
// final_provider is a CPA credential, so it has to load without being read as a real channel.
func TestWarmupReadsV1Lines(t *testing.T) {
	directory := t.TempDir()
	now := time.Now().UTC()
	line := `{"v":1,"time":"` + now.Add(-time.Hour).UTC().Format(time.RFC3339) + `","request_id":"legacy-request",` +
		`"session_id":"legacy-session","generation_id":"legacy-generation","model":"deepseek-flash-1",` +
		`"upstream_model":"deepseek/deepseek-v4.1-flash","canonical_slug":"deepseek/deepseek-v4.1-flash",` +
		`"final_provider":"deepseek","resolved_provider":"deepseek","pinned_provider":"deepseek",` +
		`"affinity":"confirmed","upstream_request_id":"legacy-upstream","model_attempt_count":1,` +
		`"total_provider_attempt_count":1,"protocol":"openai","stream":true,"status_code":200,"ttft_ms":1200,` +
		`"duration_ms":3100,"decode_ms":1900,"frames":5,"input_tokens":800,"output_tokens":200,` +
		`"reasoning_tokens":200,"cached_tokens":512,"total_tokens":1000,"cost_usd":0.0012}` + "\n"
	writeStoredLine(t, directory, now, line)

	recorder := New(Options{
		Enabled:       true,
		Directory:     directory,
		RetentionDays: 3,
		MaxSizeMB:     16,
		Baseline:      providerBaseline,
	})
	recorder.Start()
	t.Cleanup(recorder.Stop)

	health := recorder.Health()
	if health.Warmup.Records != 1 {
		t.Errorf("warmup records = %d, want the one v1 line", health.Warmup.Records)
	}
	if health.LastError != "" {
		t.Errorf("reading a v1 line reported an error: %s", health.LastError)
	}
	records := recordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want the v1 line back, got %d records", len(records))
	}
	record := records[0]
	if record.Schema != 1 || record.RequestID != "legacy-request" {
		t.Errorf("record = %+v, want the v1 line unchanged", record)
	}
	if record.CPProvider != providerBaseline || record.PinnedProvider != providerBaseline || record.CostUSD != 0.0012 {
		t.Errorf("the v1 keys did not survive the round trip: %+v", record)
	}
	// The v1 source named the CPA credential with the names v3 gives to the gateway channel.
	if record.FinalProvider != "" || record.ResolvedProvider != "" || record.GatewayProvider != "" {
		t.Errorf("record = %+v, want the v1 final_provider read as the credential, not as a channel", record)
	}
	if summary := recorder.Summary(mustWindow(t, "24h")); summary.Unresolved != 1 || summary.Resolved != 0 {
		t.Errorf("summary resolved/unresolved = %d/%d, want 0/1: a v1 line has no channel",
			summary.Resolved, summary.Unresolved)
	}
}

// TestHandleUsageCountsDecodeFailures is the diagnostic path: a payload this build cannot read
// is counted, never stored and never allowed to fail the call.
func TestHandleUsageCountsDecodeFailures(t *testing.T) {
	recorder, _ := newRecorder(t)
	useRecorder(t, recorder)

	for _, broken := range [][]byte{[]byte(`{"Detail":`), []byte(`{"Latency":"soon"}`)} {
		answer, errHandle := HandleUsage(broken)
		if errHandle != nil {
			t.Fatalf("HandleUsage(%q) returned an error: %v", broken, errHandle)
		}
		decodeKeepAnswer(t, answer, "HandleUsage(broken)")
	}

	health := recorder.Health()
	if health.DecodeFailures != 2 {
		t.Errorf("decode_failures = %d, want 2", health.DecodeFailures)
	}
	if health.LastDecodeError == "" || health.LastDecodeErrorAt == nil {
		t.Errorf("the last decode error was not recorded: %+v", health)
	}
	if health.Events != 0 || health.Written != 0 || health.FailedEvents != 0 {
		t.Errorf("health = %+v, want nothing accepted or written", health)
	}
	if records := recordsOf(t, recorder); len(records) != 0 {
		t.Errorf("stored %d records for undecodable payloads", len(records))
	}
}

// TestHandleUsageAlwaysKeepsAndNeverFails is the fail-open guard: whatever arrives — empty
// bytes, junk, a truncated object, a payload from a host build this plugin does not know, or
// no recorder at all — the answer is the keep envelope and the error is nil.
func TestHandleUsageAlwaysKeepsAndNeverFails(t *testing.T) {
	recorder, _ := newRecorder(t)
	useRecorder(t, recorder)

	for _, input := range [][]byte{
		nil,
		{},
		[]byte("data: not json"),
		[]byte(`{"Provider":"x","Unknown":{"nested":[1,2,3]}}`),
		[]byte(`{"Provider":"x","Latency":1.5}`),
	} {
		answer, errHandle := HandleUsage(input)
		if errHandle != nil {
			t.Fatalf("HandleUsage(%q) returned an error: %v", input, errHandle)
		}
		decodeKeepAnswer(t, answer, "HandleUsage")
	}

	// A plugin that is loaded with observation switched off has no recorder installed; the
	// host must still get the keep answer.
	SetActive(nil)
	if answer, errInactive := HandleUsage([]byte(liveUsagePayload)); errInactive != nil {
		t.Fatalf("HandleUsage with no active recorder returned an error: %v", errInactive)
	} else {
		decodeKeepAnswer(t, answer, "HandleUsage(inactive)")
	}
}

// TestRecorderSurvivesAnUnwritableStore keeps the failure containment: a store that cannot be
// written still counts the request, still reports the reason, and still answers the host. The
// window is empty afterwards, and that is the honest answer — the record IS the store, so a
// request the store refused is a request no view can report.
func TestRecorderSurvivesAnUnwritableStore(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "not-a-directory")
	if errWrite := os.WriteFile(directory, []byte("occupied"), 0o644); errWrite != nil {
		t.Fatalf("prepare file: %v", errWrite)
	}
	recorder := New(Options{Enabled: true, Directory: directory, RetentionDays: 3, MaxSizeMB: 16, Baseline: providerBaseline})
	recorder.Start()
	t.Cleanup(recorder.Stop)

	if health := recorder.Health(); health.LastError == "" {
		t.Fatalf("health did not report the unusable directory: %+v", health)
	}
	useRecorder(t, recorder)

	wire := usageFixture(providerBaseline, time.Now().UTC())
	raw, errMarshal := json.Marshal(wire)
	if errMarshal != nil {
		t.Fatalf("marshal payload: %v", errMarshal)
	}
	if _, errHandle := HandleUsage(raw); errHandle != nil {
		t.Fatalf("HandleUsage returned an error with an unusable store: %v", errHandle)
	}
	recorder.Stop()

	health := recorder.Health()
	if health.WriteFailures == 0 {
		t.Errorf("write_failures = 0, want the failed append to be counted: %+v", health)
	}
	if health.Events != 1 {
		t.Errorf("events = %d, want 1: the request was still observed", health.Events)
	}
	summary := recorder.Summary(mustWindow(t, "24h"))
	if summary.Resolved != 0 || summary.Unresolved != 0 {
		t.Errorf("summary = %+v, want an empty window: nothing reached the store", summary)
	}
	if summary.Health.LastError == "" {
		t.Errorf("summary.health carries no reason for the empty window: %+v", summary.Health)
	}
}

// TestSummaryLabelsEveryHourOnce keeps the timeline contract: one point per absolute hour,
// all labelled the same way, and a filled hour carries the timings of its requests.
func TestSummaryLabelsEveryHourOnce(t *testing.T) {
	recorder, clock := newRecorder(t)
	first := usageFixture(providerBaseline, clock.at.Add(-time.Minute))
	first.RequestID = "req-hour-a"
	feedUsage(t, recorder, first)
	clock.advance(30 * time.Minute)
	second := usageFixture(providerBaseline, clock.at.Add(-time.Minute))
	second.RequestID = "req-hour-b"
	feedUsage(t, recorder, second)

	summary := recorder.Summary(mustWindow(t, "24h"))

	seen := map[int64]bool{}
	filled := 0
	for _, point := range summary.Hours {
		if seen[point.Hour.Unix()] {
			t.Fatalf("hour %s appears twice in the timeline", point.Hour)
		}
		seen[point.Hour.Unix()] = true
		if point.Hour.Location() != summary.From.Location() {
			t.Errorf("hour %s is labelled in %s, want the window's %s",
				point.Hour, point.Hour.Location(), summary.From.Location())
		}
		if point.Requests > 0 {
			filled++
			if point.TTFTP50Ms <= 0 || point.DecodeP50 <= 0 {
				t.Errorf("hour %s carries %d requests but no timing: %+v", point.Hour, point.Requests, point)
			}
		}
	}
	if filled == 0 {
		t.Fatal("no hour carries the two recorded requests")
	}
	if len(summary.Hours) != 25 {
		t.Errorf("timeline has %d points, want 25 for a rolling 24h window", len(summary.Hours))
	}
	// A model row without its percentiles reads as a zero through the API, which looks like a
	// stalled stream rather than a missing field.
	if len(summary.Models) == 0 {
		t.Fatal("no model row")
	}
	for _, model := range summary.Models {
		if model.Model != "cline-pass/deepseek-v4.1-flash" {
			t.Errorf("model row = %+v, want the model the client asked for", model)
		}
		if model.TTFTP50Ms <= 0 || model.DecodeP50 <= 0 {
			t.Errorf("model row %+v has no timing, want the percentiles of its requests", model)
		}
	}
}

func TestParseWindow(t *testing.T) {
	for _, check := range []struct {
		raw  string
		want time.Duration
		ok   bool
	}{
		{"", 24 * time.Hour, true},
		{"1h", time.Hour, true},
		{"24h", 24 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"30d", 0, false},
		{"nonsense", 0, false},
	} {
		window, ok := ParseWindow(check.raw)
		if ok != check.ok {
			t.Errorf("ParseWindow(%q) ok = %v, want %v", check.raw, ok, check.ok)
			continue
		}
		if ok && window.Duration() != check.want {
			t.Errorf("ParseWindow(%q) = %v, want %v", check.raw, window.Duration(), check.want)
		}
	}
}

func mustWindow(t *testing.T, raw string) Window {
	t.Helper()
	window, ok := ParseWindow(raw)
	if !ok {
		t.Fatalf("ParseWindow(%q) failed", raw)
	}
	return window
}

// csvLines exports the 24h window and returns the header and one line per record.
func csvLines(t *testing.T, recorder *Recorder) []string {
	t.Helper()
	recorder.Flush()
	buffer := &bytes.Buffer{}
	rows, errWrite := recorder.WriteCSV(mustWindow(t, "24h"), buffer)
	if errWrite != nil {
		t.Fatalf("WriteCSV: %v", errWrite)
	}
	if rows != 1 {
		t.Fatalf("WriteCSV wrote %d rows, want 1", rows)
	}
	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a header and one row, got %d lines: %q", len(lines), buffer.String())
	}
	return lines
}

// csvColumn re-exports the window and returns one column of its first data row.
func csvColumn(t *testing.T, recorder *Recorder, name string) string {
	t.Helper()
	lines := csvLines(t, recorder)
	header := strings.Split(lines[0], ",")
	row := strings.Split(lines[1], ",")
	for index, column := range header {
		if column == name {
			return row[index]
		}
	}
	t.Fatalf("the export has no %s column: %v", name, header)
	return ""
}

// csvExport exports the 24h window and returns the header and every data row, so a test can
// assert on a window with more than one record.
func csvExport(t *testing.T, recorder *Recorder) ([]string, [][]string) {
	t.Helper()
	recorder.Flush()
	buffer := &bytes.Buffer{}
	if _, errWrite := recorder.WriteCSV(mustWindow(t, "24h"), buffer); errWrite != nil {
		t.Fatalf("WriteCSV: %v", errWrite)
	}
	reader := csv.NewReader(strings.NewReader(buffer.String()))
	rows, errRead := reader.ReadAll()
	if errRead != nil {
		t.Fatalf("the export must parse as CSV: %v (%q)", errRead, buffer.String())
	}
	if len(rows) == 0 {
		t.Fatalf("the export carries no header: %q", buffer.String())
	}
	header := rows[0]
	for index, row := range rows[1:] {
		if len(row) != len(header) {
			t.Fatalf("data row %d has %d fields, the header has %d", index, len(row), len(header))
		}
	}
	return header, rows[1:]
}

// csvColumnIndex maps the header to the position of each column.
func csvColumnIndex(header []string) map[string]int {
	columns := make(map[string]int, len(header))
	for index, name := range header {
		columns[name] = index
	}
	return columns
}

// storedLine returns the raw JSONL line of one stored record, so a test can assert on the keys
// the wire actually carries rather than on the struct it decoded into.
func storedLine(t *testing.T, recorder *Recorder, requestID string) string {
	t.Helper()
	recorder.Flush()
	for _, entry := range recorder.store.list() {
		content, errRead := os.ReadFile(entry.path)
		if errRead != nil {
			t.Fatalf("read %s: %v", entry.path, errRead)
		}
		for _, line := range strings.Split(string(content), "\n") {
			if strings.Contains(line, `"request_id":"`+requestID+`"`) {
				return line
			}
		}
	}
	t.Fatalf("no stored line for request %q", requestID)
	return ""
}

// writeStoredLine writes one pre-serialised record into the day file a store would use.
func writeStoredLine(t *testing.T, directory string, date time.Time, line string) {
	t.Helper()
	path := filepath.Join(directory, filePrefix+date.UTC().Format(dateLayout)+fileSuffix)
	if errWrite := os.WriteFile(path, []byte(line), 0o644); errWrite != nil {
		t.Fatalf("write %s: %v", path, errWrite)
	}
}
