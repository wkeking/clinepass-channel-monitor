package observation

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fixtures below mirror what the Cline gateway actually sends on this host: an OpenAI
// chat stream whose terminating frame carries the channel report, taken from a real
// deepseek-flash-1 capture. provider_metadata sits inside choices[0].delta, and the usage
// object rides the same frame.

const (
	frameRoleOnly = `data: {"id":"chatcmpl-fixture","object":"chat.completion.chunk","model":"deepseek-flash-1","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"

	frameReasoning = `data: {"id":"chatcmpl-fixture","object":"chat.completion.chunk","model":"deepseek-flash-1","choices":[{"index":0,"delta":{"reasoning":"先看目录结构"}}]}` + "\n\n"

	// frameRouting is the normal case: the channel report of a request that landed where it
	// was planned to land.
	frameRouting = `data: {"id":"chatcmpl-fixture","object":"chat.completion.chunk","model":"deepseek-flash-1","choices":[{"index":0,"delta":{"provider_metadata":{"gateway":{"routing":{"finalProvider":"deepseek","resolvedProvider":"deepseek","canonicalSlug":"deepseek/deepseek-v4.1-flash","originalModelId":"deepseek/deepseek-v4.1-flash","clientSessionId":"sess-fixture-1","generationId":"gen-fixture-1","modelAttemptCount":1,"totalProviderAttemptCount":1,"affinity":{"outcome":"confirmed","pinnedProvider":"deepseek"},"fallbacksAvailable":[],"planningReasoning":"Provider set restricted to: deepseek.","modelAttempts":[{"modelId":"deepseek/deepseek-v4.1-flash","providerAttempts":[{"provider":"deepseek","providerRequestId":"prov-fixture-1","statusCode":200,"success":true}]}]}}}}}],"usage":{"prompt_tokens":800,"completion_tokens":200,"total_tokens":1000,"cost":0.0012,"is_byok":false,"prompt_tokens_details":{"cached_tokens":512},"completion_tokens_details":{"reasoning_tokens":200}}}` + "\n\n"

	// frameRoutingFallback is the sample the requirement asks for: the gateway reports a
	// final provider that differs from the resolved one, which is how a fallback onto a
	// non-official channel looks. canonicalSlug stays deepseek/... — recording the slug
	// alone would have hidden this request, which is why the channel is observed at all.
	frameRoutingFallback = `data: {"id":"chatcmpl-fallback","object":"chat.completion.chunk","model":"deepseek-flash-1","choices":[{"index":0,"delta":{"provider_metadata":{"gateway":{"routing":{"finalProvider":"alibaba","resolvedProvider":"deepseek","canonicalSlug":"deepseek/deepseek-v4.1-flash","originalModelId":"deepseek/deepseek-v4.1-flash","clientSessionId":"sess-fixture-2","generationId":"gen-fixture-2","modelAttemptCount":1,"totalProviderAttemptCount":3,"affinity":{"outcome":"fallback","pinnedProvider":"deepseek"},"fallbacksAvailable":["particle"],"modelAttempts":[{"modelId":"deepseek/deepseek-v4.1-flash","providerAttempts":[{"provider":"deepseek","providerRequestId":"prov-fixture-2","statusCode":429},{"provider":"particle","providerRequestId":"prov-fixture-3","statusCode":503},{"provider":"alibaba","providerRequestId":"prov-fixture-4","statusCode":200}]}]}}}}}],"usage":{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500,"cost":0.0031,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":300}}}` + "\n\n"

	// frameUsageOnly has token counters but no channel report: the shape of a response the
	// gateway finished without saying where it came from.
	frameUsageOnly = `data: {"id":"chatcmpl-noroute","object":"chat.completion.chunk","model":"deepseek-flash-1","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":40,"completion_tokens":7,"total_tokens":47}}` + "\n\n"

	framePlainText = `data: {"id":"chatcmpl-noroute","object":"chat.completion.chunk","model":"deepseek-flash-1","choices":[{"index":0,"delta":{"content":"/tmp"}}]}` + "\n\n"

	frameDone = "data: [DONE]\n\n"

	// frameTextMentioningRouting carries the needle word in the model's own output. The shape
	// is real: an agent discussing this very plugin writes "provider_metadata" in its answer,
	// and such a chunk must never be read as a channel report cut across two boundaries.
	frameTextMentioningRouting = `data: {"id":"chatcmpl-mention","object":"chat.completion.chunk","model":"deepseek-flash-1","choices":[{"index":0,"delta":{"reasoning":"I will read provider_metadata.gateway.routing from the frame"}}]}` + "\n\n"

	// frameResponsesText and frameResponsesCompleted replay the Responses API, which is what
	// the codex client speaks through CPA. Its stream carries no provider_metadata at all, so
	// the channel of such a request cannot be observed: it only ever counts as unresolved.
	frameResponsesText      = `data: {"type":"response.output_text.delta","delta":"reading the routing table"}` + "\n\n"
	frameResponsesCompleted = `data: {"type":"response.completed","response":{"id":"resp-fixture","usage":{"input_tokens":900,"output_tokens":120,"total_tokens":1020,"input_tokens_details":{"cached_tokens":128},"output_tokens_details":{"reasoning_tokens":40}}}}` + "\n\n"
)

// testClock makes ttft, duration and the hourly buckets deterministic.
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

func newRecorder(t *testing.T) (*Recorder, *testClock) {
	t.Helper()
	clock := &testClock{at: time.Now().UTC().Truncate(time.Second)}
	recorder := New(Options{
		Enabled:       true,
		Directory:     t.TempDir(),
		RetentionDays: 3,
		MaxSizeMB:     16,
		Baseline:      "deepseek",
	})
	recorder.nowFn = clock.now
	recorder.Start()
	t.Cleanup(recorder.Stop)
	return recorder, clock
}

// feedStream replays one streamed request the way the host does: a header-init call, then
// one call per payload chunk. The timing is deliberate — the first text frame arrives 1.2 s
// after the response started, the channel report 2.2 s after it — so ttft and decode speed
// are checkable numbers instead of zeroes.
func feedStream(t *testing.T, recorder *Recorder, clock *testClock, requestID, model string, frames ...string) {
	t.Helper()
	recorder.Observe(&InterceptRequest{
		RequestID:       requestID,
		Model:           model,
		RequestedModel:  model,
		SourceFormat:    "openai",
		RequestHeaders:  map[string][]string{"User-Agent": {"claude-cli/2.1.0"}, "X-App": {"cline-cli"}},
		ResponseHeaders: map[string][]string{"X-Upstream-Status": {"200"}},
		ChunkIndex:      ChunkHeaderInitIndex,
	})
	for index, body := range frames {
		switch index {
		case 1:
			clock.advance(1200 * time.Millisecond)
		case 2:
			clock.advance(100 * time.Millisecond)
		default:
			if index > 0 {
				clock.advance(900 * time.Millisecond)
			}
		}
		recorder.Observe(&InterceptRequest{RequestID: requestID, ChunkIndex: index, Body: []byte(body)})
	}
}

func recordsOf(t *testing.T, recorder *Recorder) []Record {
	t.Helper()
	// The writer is asynchronous, so the records a test just fed are only on disk after a
	// flush: the page reads the same file this reads.
	recorder.Flush()
	window, ok := ParseWindow("24h")
	if !ok {
		t.Fatalf("ParseWindow(24h) failed")
	}
	records, errLoad := recorder.RecordsSince(window)
	if errLoad != nil {
		t.Fatalf("RecordsSince: %v", errLoad)
	}
	return records
}

func TestObserveRecordsChannelFromRoutingFrame(t *testing.T) {
	recorder, clock := newRecorder(t)
	feedStream(t, recorder, clock, "req-normal", "deepseek-flash-1",
		frameRoleOnly, frameReasoning, framePlainText, framePlainText, frameRouting, frameDone)

	records := recordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want exactly 1 record, got %d: %+v", len(records), records)
	}
	record := records[0]
	for _, check := range []struct {
		name string
		got  any
		want any
	}{
		{"v", record.Schema, schemaVersion},
		{"request_id", record.RequestID, "req-normal"},
		{"session_id", record.SessionID, "sess-fixture-1"},
		{"generation_id", record.GenerationID, "gen-fixture-1"},
		{"model", record.Model, "deepseek-flash-1"},
		{"canonical_slug", record.CanonicalSlug, "deepseek/deepseek-v4.1-flash"},
		{"original_model_id", record.OriginalModel, "deepseek/deepseek-v4.1-flash"},
		{"final_provider", record.FinalProvider, "deepseek"},
		{"resolved_provider", record.ResolvedProvider, "deepseek"},
		{"pinned_provider", record.PinnedProvider, "deepseek"},
		{"affinity", record.AffinityOutcome, "confirmed"},
		{"upstream_request_id", record.UpstreamRequestID, "prov-fixture-1"},
		{"model_attempt_count", record.ModelAttempts, 1},
		{"total_provider_attempt_count", record.Attempts, 1},
		{"protocol", record.Protocol, "openai"},
		{"stream", record.Stream, true},
		{"status_code", record.StatusCode, 200},
		{"ttft_ms", record.TTFTMs, int64(1200)},
		{"duration_ms", record.DurationMs, int64(3100)},
		{"decode_ms", record.DecodeMs, int64(1900)},
		{"frames", record.Frames, 5},
		{"input_tokens", record.InputTokens, int64(800)},
		{"output_tokens", record.OutputTokens, int64(200)},
		{"reasoning_tokens", record.ReasoningTokens, int64(200)},
		{"cached_tokens", record.CachedTokens, int64(512)},
		{"total_tokens", record.TotalTokens, int64(1000)},
		{"cost_usd", record.CostUSD, 0.0012},
		{"user_agent", record.UserAgent, "claude-cli/2.1.0"},
		{"client_app", record.ClientApp, "cline-cli"},
	} {
		if check.got != check.want {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
	if want := 200.0 / 1.9; record.TPS < want-0.01 || record.TPS > want+0.01 {
		t.Errorf("tokens_per_second = %v, want %v", record.TPS, want)
	}
	if len(record.Fallbacks) != 0 {
		t.Errorf("fallbacks_available = %v, want empty", record.Fallbacks)
	}

	health := recorder.Health()
	if health.Resolved != 1 || health.Written != 1 || health.Unresolved != 0 || health.ParseFailures != 0 {
		t.Errorf("health = %+v, want resolved 1, written 1, unresolved 0, parse_failures 0", health)
	}
	if health.PendingStreams != 0 {
		t.Errorf("pending_streams = %d, want 0 after the routing frame", health.PendingStreams)
	}
	if health.LastRecordAt == nil || health.LastWriteAt == nil {
		t.Errorf("health timestamps not set: %+v", health)
	}
}

func TestObserveWithoutProviderMetadataIsUnresolvedAndNotStored(t *testing.T) {
	recorder, clock := newRecorder(t)
	// No terminator: this is a stream that just stops, which is why the janitor has to be
	// able to forget it.
	feedStream(t, recorder, clock, "req-noroute", "deepseek-flash-1",
		framePlainText, frameUsageOnly)

	// The stream ended without a channel report. The request stays pending until the janitor
	// forgets it, and is counted as unresolved rather than guessed at.
	health := recorder.Health()
	if health.PendingStreams != 1 {
		t.Fatalf("pending_streams = %d, want 1", health.PendingStreams)
	}
	clock.advance(pendingTTL + time.Minute)
	if swept := recorder.sweepPending(); swept != 1 {
		t.Fatalf("sweepPending = %d, want 1", swept)
	}

	health = recorder.Health()
	if health.Unresolved != 1 {
		t.Errorf("unresolved = %d, want 1", health.Unresolved)
	}
	if health.Resolved != 0 || health.Written != 0 || health.Dropped != 0 {
		t.Errorf("health = %+v, want nothing resolved, written or dropped", health)
	}
	// A frame with no channel report is normal traffic, not a parse failure: the host calls
	// this interceptor for every upstream, not only for Cline.
	if health.ParseFailures != 0 {
		t.Errorf("parse_failures = %d, want 0", health.ParseFailures)
	}
	if records := recordsOf(t, recorder); len(records) != 0 {
		t.Errorf("stored %d records for a request with no channel report", len(records))
	}
}

func TestObserveTerminatorCountsUnresolvedWithoutWaiting(t *testing.T) {
	recorder, clock := newRecorder(t)
	feedStream(t, recorder, clock, "req-terminated", "deepseek-flash-1",
		framePlainText, frameUsageOnly, frameDone)

	// The terminator is what says the response is over, so the request is counted as
	// unresolved right away instead of waiting pendingTTL for the janitor: a count that only
	// moves fifteen minutes after the facts cannot be watched.
	health := recorder.Health()
	if health.PendingStreams != 0 {
		t.Errorf("pending_streams = %d, want 0 once the terminator arrived", health.PendingStreams)
	}
	if health.Unresolved != 1 {
		t.Errorf("unresolved = %d, want 1", health.Unresolved)
	}
	if health.Resolved != 0 || health.Written != 0 || health.Dropped != 0 {
		t.Errorf("health = %+v, want nothing resolved, written or dropped", health)
	}
	if records := recordsOf(t, recorder); len(records) != 0 {
		t.Errorf("stored %d records for a request with no channel report", len(records))
	}
}

func TestObserveResponsesStreamIsUnresolved(t *testing.T) {
	recorder, clock := newRecorder(t)
	// The Responses API never reports the channel: its frames carry no provider_metadata at
	// all, which /tmp/responses_dump.py confirmed against real codex traffic. Such a request
	// can only be counted as unresolved, and the counter has to make that visible.
	feedStream(t, recorder, clock, "req-responses", "deepseek-flash-2",
		frameResponsesText, frameResponsesText, frameResponsesCompleted)

	health := recorder.Health()
	if health.Unresolved != 1 || health.PendingStreams != 0 {
		t.Errorf("health = %+v, want 1 unresolved and no pending stream", health)
	}
	if health.Written != 0 {
		t.Errorf("written = %d, want 0: a request without a channel is not a record", health.Written)
	}
	if records := recordsOf(t, recorder); len(records) != 0 {
		t.Errorf("stored %d records for a stream that never reported a channel", len(records))
	}
}

func TestObserveTextMentionOfNeedleKeepsTheRecord(t *testing.T) {
	recorder, clock := newRecorder(t)
	// The model writes the needle word four times -- more than the retry budget -- before the
	// real channel report arrives. Reading those chunks as a routing block split across
	// boundaries would spend the budget, give the stream up, and lose the record; the request
	// would then sit in the pending map until the janitor swept it.
	feedStream(t, recorder, clock, "req-mention", "deepseek-flash-1",
		framePlainText, frameTextMentioningRouting, frameTextMentioningRouting,
		frameTextMentioningRouting, frameTextMentioningRouting, frameRouting)

	records := recordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want 1 record although the text mentioned the needle, got %d: %+v", len(records), records)
	}
	if records[0].FinalProvider != "deepseek" || records[0].SessionID != "sess-fixture-1" {
		t.Errorf("record = %+v, want final_provider deepseek and session sess-fixture-1", records[0])
	}
	health := recorder.Health()
	if health.ParseFailures != 0 {
		t.Errorf("parse_failures = %d, want 0: a word in the answer is not a parse failure", health.ParseFailures)
	}
	if health.NeedleMisses < 4 {
		t.Errorf("needle_misses = %d, want at least 4", health.NeedleMisses)
	}
}

func TestObserveSplitRoutingFrame(t *testing.T) {
	recorder, clock := newRecorder(t)
	// The routing frame arrives cut in two, which is what a chunk boundary inside a large
	// SSE frame looks like. The second half cannot be decoded on its own; the recorder has
	// to retry with the previous chunk prepended.
	cut := strings.Index(frameRouting, `"clientSessionId"`)
	feedStream(t, recorder, clock, "req-split", "deepseek-flash-1",
		framePlainText, frameRouting[:cut], frameRouting[cut:])

	records := recordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want exactly 1 record from a split frame, got %d: %+v", len(records), records)
	}
	record := records[0]
	if record.FinalProvider != "deepseek" {
		t.Errorf("final_provider = %q, want deepseek", record.FinalProvider)
	}
	if record.SessionID != "sess-fixture-1" {
		t.Errorf("session_id = %q, want sess-fixture-1", record.SessionID)
	}
	if record.InputTokens != 800 || record.OutputTokens != 200 {
		t.Errorf("tokens = %d/%d, want 800/200", record.InputTokens, record.OutputTokens)
	}
	if health := recorder.Health(); health.Resolved != 1 || health.ParseFailures != 0 {
		t.Errorf("health = %+v, want 1 resolved and no parse failure", health)
	}
}

func TestObserveFallbackRecordAndSummary(t *testing.T) {
	recorder, clock := newRecorder(t)
	feedStream(t, recorder, clock, "req-fallback", "deepseek-flash-1",
		frameRoleOnly, framePlainText, framePlainText, frameRoutingFallback)
	feedStream(t, recorder, clock, "req-normal", "deepseek-flash-1",
		frameRoleOnly, framePlainText, framePlainText, frameRouting)

	records := recordsOf(t, recorder)
	if len(records) != 2 {
		t.Fatalf("want 2 records, got %d", len(records))
	}
	var fallback Record
	for _, record := range records {
		if record.FinalProvider == "alibaba" {
			fallback = record
		}
	}
	if fallback.RequestID != "req-fallback" {
		t.Fatalf("no fallback record found in %+v", records)
	}
	if fallback.FinalProvider == fallback.ResolvedProvider {
		t.Errorf("final_provider and resolved_provider are both %q; the fixture must differ",
			fallback.FinalProvider)
	}
	if fallback.ResolvedProvider != "deepseek" {
		t.Errorf("resolved_provider = %q, want deepseek", fallback.ResolvedProvider)
	}
	if len(fallback.Fallbacks) != 1 || fallback.Fallbacks[0] != "particle" {
		t.Errorf("fallbacks_available = %v, want [particle]", fallback.Fallbacks)
	}
	if fallback.Attempts != 3 {
		t.Errorf("total_provider_attempt_count = %d, want 3", fallback.Attempts)
	}
	if fallback.StatusCode != 429 {
		t.Errorf("status_code = %d, want the first provider attempt's 429", fallback.StatusCode)
	}

	window, _ := ParseWindow("24h")
	summary := recorder.Summary(window)
	if summary.Resolved != 2 || summary.OffBaseline != 1 {
		t.Fatalf("summary resolved=%d off_baseline=%d, want 2/1", summary.Resolved, summary.OffBaseline)
	}
	if summary.OffRatio != 0.5 {
		t.Errorf("off_baseline_ratio = %v, want 0.5", summary.OffRatio)
	}
	if summary.Baseline != "deepseek" {
		t.Errorf("baseline_provider = %q, want deepseek", summary.Baseline)
	}
	if summary.Channels != 2 {
		t.Errorf("channels = %d, want 2", summary.Channels)
	}
	seen := map[string]ProviderStat{}
	for _, provider := range summary.Providers {
		seen[provider.Provider] = provider
	}
	if got := seen["alibaba"]; got.Requests != 1 || got.OffBaseline != 1 || got.Ratio != 0.5 {
		t.Errorf("alibaba provider row = %+v, want 1 request, off baseline, ratio 0.5", got)
	}
	if got := seen["deepseek"]; got.Requests != 1 || got.OffBaseline != 0 {
		t.Errorf("deepseek provider row = %+v, want 1 request on baseline", got)
	}
	if got := seen["alibaba"]; got.TTFTP50Ms != 1200 {
		t.Errorf("alibaba ttft_p50_ms = %v, want 1200", got.TTFTP50Ms)
	}
	// 300 output tokens over a 1 s decode window: the per-provider rate is the same figure the
	// page shows for a single request, so it has to come out of that request's own timings.
	if got := seen["alibaba"]; got.DecodeP50 < 299.99 || got.DecodeP50 > 300.01 {
		t.Errorf("alibaba decode_p50_tps = %v, want 300", got.DecodeP50)
	}
	// The hourly timeline has to be dense: a chart with a hole in it would read as "no
	// requests" when the hour had none rather than as a gap. The window starts inside an
	// hour, so it takes 25 marks for 24 hours: the first and last bar cover a part of their
	// hour, and their sums still add up to the window total because the buckets are the same
	// ones the totals were read from.
	if len(summary.Hours) != 25 {
		t.Errorf("hours = %d, want one point per hour mark of the window", len(summary.Hours))
	}
	first, last := summary.Hours[0], summary.Hours[len(summary.Hours)-1]
	if !first.Hour.Equal(summary.From.Truncate(time.Hour)) || !last.Hour.Equal(summary.To.Truncate(time.Hour)) {
		t.Errorf("timeline runs %v..%v, want %v..%v", first.Hour, last.Hour,
			summary.From.Truncate(time.Hour), summary.To.Truncate(time.Hour))
	}
}

func TestWriteCSVKeepsChannelColumns(t *testing.T) {
	recorder, clock := newRecorder(t)
	feedStream(t, recorder, clock, "req-fallback", "deepseek-flash-1",
		framePlainText, frameRoutingFallback)

	window, _ := ParseWindow("24h")
	recorder.Flush()
	buffer := &bytes.Buffer{}
	rows, errWrite := recorder.WriteCSV(window, buffer)
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
	header := strings.Split(lines[0], ",")
	row := strings.Split(lines[1], ",")
	if len(header) != len(row) {
		t.Fatalf("header has %d columns, row has %d", len(header), len(row))
	}
	column := map[string]string{}
	for index, name := range header {
		column[name] = row[index]
	}
	for _, name := range []string{"final_provider", "resolved_provider", "pinned_provider", "canonical_slug", "off_baseline"} {
		if _, ok := column[name]; !ok {
			t.Fatalf("CSV has no %s column: %v", name, header)
		}
	}
	if column["final_provider"] != "alibaba" || column["resolved_provider"] != "deepseek" {
		t.Errorf("channel columns = %q/%q, want alibaba/deepseek", column["final_provider"], column["resolved_provider"])
	}
	// The export is asked "which requests left the official channel", so a row that landed
	// on another provider has to be marked as such in the file itself.
	if column["off_baseline"] != "yes" {
		t.Errorf("off_baseline = %q, want yes for the alibaba row", column["off_baseline"])
	}
}

func TestHandleKeepsChunkAndNeverFails(t *testing.T) {
	recorder, clock := newRecorder(t)
	SetActive(recorder)
	t.Cleanup(func() { SetActive(nil) })

	valid := &InterceptRequest{
		RequestID:       "req-handle",
		Model:           "deepseek-flash-1",
		SourceFormat:    "openai",
		RequestHeaders:  map[string][]string{"User-Agent": {"claude-cli/2.1.0"}},
		ResponseHeaders: map[string][]string{"X-Upstream-Status": {"200"}},
		ChunkIndex:      ChunkHeaderInitIndex,
	}
	raw, errMarshal := json.Marshal(valid)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	answer, errHandle := Handle(raw)
	if errHandle != nil {
		t.Fatalf("Handle returned an error: %v", errHandle)
	}
	var envelope struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(answer, &envelope); errUnmarshal != nil {
		t.Fatalf("answer is not the envelope the host expects: %v (%s)", errUnmarshal, answer)
	}
	if !envelope.OK {
		t.Fatalf("answer ok=false: %s", answer)
	}
	// An empty result means "keep this chunk": anything else would rewrite client traffic.
	var change InterceptResponse
	if errUnmarshal := json.Unmarshal(envelope.Result, &change); errUnmarshal != nil {
		t.Fatalf("result is not an intercept response: %v (%s)", errUnmarshal, envelope.Result)
	}
	if change.DropChunk || len(change.Body) != 0 || len(change.Headers) != 0 || len(change.ClearHeaders) != 0 {
		t.Fatalf("the plugin answered with a change: %+v", change)
	}

	// Unparseable input, and no recorder installed at all: both must still answer "keep".
	for _, broken := range [][]byte{nil, []byte("data: not json"), []byte(`{"RequestID":`)} {
		if _, errBroken := Handle(broken); errBroken != nil {
			t.Fatalf("Handle(%q) returned an error: %v", broken, errBroken)
		}
	}
	SetActive(nil)
	if _, errInactive := Handle(raw); errInactive != nil {
		t.Fatalf("Handle with no active recorder returned an error: %v", errInactive)
	}
	clock.advance(time.Second)
}

func TestRecorderSurvivesAnUnwritableStore(t *testing.T) {
	// A file where the directory should be: opening the store fails, and the failure must
	// stay inside the collector. The answer to the host is still "keep the chunk".
	directory := filepath.Join(t.TempDir(), "not-a-directory")
	if errWrite := os.WriteFile(directory, []byte("occupied"), 0o644); errWrite != nil {
		t.Fatalf("prepare file: %v", errWrite)
	}
	clock := &testClock{at: time.Now().UTC().Truncate(time.Second)}
	recorder := New(Options{Enabled: true, Directory: directory, RetentionDays: 3, MaxSizeMB: 16, Baseline: "deepseek"})
	recorder.nowFn = clock.now
	recorder.Start()
	t.Cleanup(recorder.Stop)

	health := recorder.Health()
	if health.LastError == "" {
		t.Fatalf("health did not report the unusable directory: %+v", health)
	}
	if health.Enabled != true {
		t.Errorf("enabled = %v, want true", health.Enabled)
	}

	SetActive(recorder)
	t.Cleanup(func() { SetActive(nil) })
	raw, _ := json.Marshal(&InterceptRequest{RequestID: "req-unwritable", ChunkIndex: 0, Body: []byte(frameRouting)})
	if _, errHandle := Handle(raw); errHandle != nil {
		t.Fatalf("Handle returned an error with an unusable store: %v", errHandle)
	}
	recorder.Stop()

	health = recorder.Health()
	if health.WriteFailures == 0 {
		t.Errorf("write_failures = 0, want the failed append to be counted: %+v", health)
	}
	if health.Resolved != 1 {
		t.Errorf("resolved = %d, want 1: the channel was still observed", health.Resolved)
	}
	summary := recorder.Summary(mustWindow(t, "24h"))
	if summary.Resolved != 1 || summary.OffBaseline != 0 {
		t.Errorf("summary = %+v, want the in-memory aggregate to keep working", summary)
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

func TestObserveLeavesSpeedUnmeasuredForABatchedStream(t *testing.T) {
	recorder, clock := newRecorder(t)
	clock.advance(time.Second)
	recorder.Observe(&InterceptRequest{
		RequestID:    "req-batched",
		Model:        "deepseek-flash-1",
		SourceFormat: "openai",
		ChunkIndex:   ChunkHeaderInitIndex,
	})
	clock.advance(10 * time.Millisecond)
	recorder.Observe(&InterceptRequest{RequestID: "req-batched", ChunkIndex: 1, Body: []byte(framePlainText)})
	// The rest of the answer, routing frame included, is handed over in one batch.
	clock.advance(4 * time.Millisecond)
	recorder.Observe(&InterceptRequest{RequestID: "req-batched", ChunkIndex: 2, Body: []byte(frameRouting)})

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
	// Only the derived speed is dropped: the window and the tokens stay in the record.
	if record.OutputTokens == 0 || record.TTFTMs == 0 {
		t.Errorf("record lost its raw numbers: %+v", record)
	}
	if summary := recorder.Summary(mustWindow(t, "24h")); summary.DecodeP50TPS != 0 {
		t.Errorf("window decode_p50_tps = %v, want 0: an unmeasured speed must not reach the page",
			summary.DecodeP50TPS)
	}
}

func TestSummaryLabelsEveryHourOnce(t *testing.T) {
	recorder, clock := newRecorder(t)
	feedStream(t, recorder, clock, "req-hour-a", "deepseek-flash-1",
		frameRoleOnly, framePlainText, frameRouting)
	clock.advance(30 * time.Minute)
	feedStream(t, recorder, clock, "req-hour-b", "deepseek-flash-1",
		frameRoleOnly, framePlainText, frameRouting)

	summary := recorder.Summary(mustWindow(t, "24h"))

	// One point per absolute hour, all labelled the same way. When a bucket that holds data is
	// labelled in a different location from the empty points of the same hour, the timeline
	// lists that hour twice and the chart draws the traffic at the wrong place.
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
		if model.TTFTP50Ms <= 0 || model.DecodeP50 <= 0 {
			t.Errorf("model row %+v has no timing, want the percentiles of its requests", model)
		}
	}
}
