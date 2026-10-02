package management

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/channellog"
	"github.com/wkeking/clinepass-channel-monitor/internal/observation"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// The 「渠道」 view is three things that have to agree: the page's static skeleton, the JSON
// behind it (/channel) and the export beside it (/channel.csv). One completed request is fed
// through a real recorder over the same usage.handle path a deployment uses, and a parsed
// request-log fact is put beside it, because the view answers with both halves: the CPA
// credential the usage hook reported and the gateway channel only the log knows.

const (
	// channelFixtureBaseline is the provider the recorder is configured to treat as the
	// official channel, and it is also the CPA credential the fixture request reports. Both
	// dimensions carry the same name here on purpose: the join test below then makes the fact
	// name a different channel, which is what shows the two tables answer different questions.
	channelFixtureBaseline = "deepseek"
	// channelFixtureRequestID identifies the one request every test here feeds.
	channelFixtureRequestID = "req-channel-fixture-1"
	// channelFixtureModel is the model the client asked for.
	channelFixtureModel = "deepseek-flash-1"
	// channelFixtureSessionUUID is the uuid behind the "Session_id: session-<uuid>" header. A
	// client that sends the header gets it recorded as "codex:session-<uuid>", which is what
	// the join matches the fact on; the fixtures that do not send one can never be joined.
	channelFixtureSessionUUID = "5e4d3c2b1a09"
	// channelFixtureGateway is the real channel the fixture fact names, which is deliberately
	// not the credential: the gateway answered on this one.
	channelFixtureGateway = "moonshot"
	// channelFixtureGatewayCost is the gateway's own cost figure for that request.
	channelFixtureGatewayCost = 0.00001515
	// channelFixtureArrivalGap is how far the log's arrival timestamp sits from the
	// usage-reported time; live traffic measures 72-218 ms.
	channelFixtureArrivalGap = 120 * time.Millisecond
)

// channelUsagePayload renders the usage.handle payload of one completed request. Provider is
// the CPA credential the request was routed to, which is what the record stores as
// cpa_provider; RequestedAt is stamped at call time so the record falls inside the window the
// handlers query. Both timings are zero on purpose: such a record carries no measured decode
// speed, which is what makes the "no tokens_per_second key" assertion below meaningful.
func channelUsagePayload() []byte {
	return channelUsagePayloadAt("sess-channel-fixture", time.Now().UTC())
}

// channelUsagePayloadAt renders the same payload with a session header the test chooses and a
// timestamp it controls, so a fact can be placed relative to the record to the millisecond.
func channelUsagePayloadAt(sessionID string, at time.Time) []byte {
	return []byte(`{"Provider":"` + channelFixtureBaseline + `","BaseURL":"https://api.cline.bot",` +
		`"ExecutorType":"OpenAICompatExecutor","Model":"` + channelFixtureModel + `","Alias":"deepseek-flash-2",` +
		`"APIKey":"sk_channel_fixture","RequestID":"` + channelFixtureRequestID + `","TraceID":"` + channelFixtureRequestID + `",` +
		`"SessionID":"` + sessionID + `","ParentSessionID":"","AuthID":"openai-compatibility:cline1:fixture",` +
		`"AuthIndex":"fixture-index","AuthType":"apikey","Source":"sk_channel_fixture","ReasoningEffort":"high",` +
		`"ServiceTier":"auto","ResponseServiceTier":"","ResponseModel":"deepseek/deepseek-v4.1-flash",` +
		`"Generate":true,"Stream":true,"Failed":false,"Failure":{"StatusCode":0,"Body":""},` +
		`"Detail":{"InputTokens":800,"OutputTokens":200,"ReasoningTokens":200,"CachedTokens":512,` +
		`"CacheReadTokens":0,"CacheCreationTokens":0,"TotalTokens":1000},` +
		`"RequestedAt":"` + at.UTC().Format(time.RFC3339) + `","Latency":0,"TTFT":0}`)
}

// channelFixtureFact is the parsed request log of the fixture request: the same session, an
// arrival timestamp 120 ms after the record's, and the gateway channel the request really
// landed on.
func channelFixtureFact(at time.Time) channellog.Fact {
	return channellog.Fact{
		Time:                      at,
		Path:                      "/v1/responses",
		Method:                    "POST",
		SessionID:                 "session-" + channelFixtureSessionUUID,
		SessionUUID:               channelFixtureSessionUUID,
		HasSession:                true,
		FinalProvider:             channelFixtureGateway,
		ResolvedProvider:          channelFixtureGateway,
		CanonicalSlug:             channelFixtureGateway + "/deepseek-v4.1-flash",
		OriginalModelID:           channelFixtureGateway + "/deepseek-v4.1-flash",
		PinnedProvider:            channelFixtureGateway,
		AffinityOutcome:           "confirmed",
		ModelAttemptCount:         1,
		TotalProviderAttemptCount: 1,
		GatewayCost:               channelFixtureGatewayCost,
		Frames:                    5,
		AttemptsSeen:              1,
		SourceFile:                "v1_responses-fixture.log",
		ParsedAt:                  at,
	}
}

// fixtureFacts is a FactSource the test fills in: the recorder reads it on every query, which
// is how a fact that arrives after the record it belongs to is still picked up.
type fixtureFacts []channellog.Fact

func (facts fixtureFacts) Facts() []channellog.Fact { return facts }

// startChannelRecorder publishes a started collector and points both entry points at it:
// the handlers read state.Observation(), the usage entry point reads observation.Active().
// The directory is returned because /health and the view both report it. Both are package
// state, so the cleanup unsets them: a leak here would decide the next test's answer.
func startChannelRecorder(t *testing.T) (*observation.Recorder, string) {
	t.Helper()
	return startChannelRecorderWithFacts(t, nil)
}

// startChannelRecorderWithFacts is startChannelRecorder with the gateway half wired in. A nil
// source is the deployment whose request-log scanner is switched off: nothing joins.
func startChannelRecorderWithFacts(t *testing.T, facts observation.FactSource) (*observation.Recorder, string) {
	t.Helper()
	loadTestConfig(defaultConfigBytes())
	directory := t.TempDir()
	recorder := observation.New(observation.Options{
		Enabled:       true,
		Directory:     directory,
		RetentionDays: 3,
		MaxSizeMB:     16,
		Baseline:      channelFixtureBaseline,
		Facts:         facts,
	})
	recorder.Start()
	t.Cleanup(func() {
		state.SetObservation(nil)
		observation.SetActive(nil)
		recorder.Stop()
	})
	state.SetObservation(recorder)
	observation.SetActive(recorder)
	return recorder, directory
}

// feedChannelRequest replays one completed request the way the host does: a single
// usage.handle call. Flush is mandatory, not cosmetic: the writer is asynchronous and the
// view reads its records back from the store.
func feedChannelRequest(t *testing.T, recorder *observation.Recorder) {
	t.Helper()
	feedChannelPayload(t, recorder, channelUsagePayload())
}

// feedChannelPayload replays one payload chosen by the test.
func feedChannelPayload(t *testing.T, recorder *observation.Recorder, payload []byte) {
	t.Helper()
	if _, errHandle := observation.HandleUsage(payload); errHandle != nil {
		t.Fatalf("HandleUsage: %v", errHandle)
	}
	recorder.Flush()
}

// decodeObject decodes one JSON object into its raw members, so a key can be asserted on the
// wire instead of through a struct this test chose the shape of.
func decodeObject(t *testing.T, raw []byte, context string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &object); errUnmarshal != nil {
		t.Fatalf("%s must be a JSON object: %v", context, errUnmarshal)
	}
	return object
}

// requireKeys fails for every member the payload is expected to carry and does not.
func requireKeys(t *testing.T, context string, object map[string]json.RawMessage, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := object[key]; !ok {
			t.Errorf("%s is missing %q", context, key)
		}
	}
}

// firstElement returns the first member of a JSON array, failing when the array is empty or
// not an array at all.
func firstElement(t *testing.T, raw []byte, context string) []byte {
	t.Helper()
	var elements []json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &elements); errUnmarshal != nil {
		t.Fatalf("%s must be a JSON array: %v", context, errUnmarshal)
	}
	if len(elements) == 0 {
		t.Fatalf("%s is empty, want the record of the fed request", context)
	}
	return elements[0]
}

// TestChannelViewIsPresent asserts the page carries the channel view's static skeleton and
// the render functions the loader wires to it. The skeleton alone is not enough: the loader
// has to ask for the payload, or the section never fills.
//
// The baseline channel name must come from the payload at render time. The page is a static
// shell that embeds no recorded data (TestIndexPageCarriesNoData enforces that), so nothing
// asserted here may name a provider.
func TestChannelViewIsPresent(t *testing.T) {
	page := string(indexHTML(nil))
	for _, id := range []string{
		"channel-section", "channel-csv", "channel-headline", "channel-cards",
		"channel-providers", "channel-models", "channel-hours", "channel-records",
		"channel-timeline",
	} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("page is missing the %s element", id)
		}
	}
	if !strings.Contains(page, "导出 CSV") {
		t.Error("page is missing the 导出 CSV button")
	}
	for _, name := range []string{"renderChannel", "channelBaselineName", "channelOfRecord", "channelIsOff"} {
		if !strings.Contains(page, "function "+name+"(") {
			t.Errorf("page is missing the %s function", name)
		}
	}
	if !strings.Contains(page, `api("/channel?window="`) {
		t.Error(`the loader must fetch the view from "/channel?window="`)
	}
	if body := jsFunctionBody(t, page, "channelBaselineName"); !strings.Contains(body, "baseline_provider") {
		t.Errorf("channelBaselineName must read the baseline from the payload, got: %s", body)
	}
	if body := jsFunctionBody(t, page, "channelIsOff"); !strings.Contains(body, "channelOfRecord(record)") {
		t.Errorf("channelIsOff must ask channelOfRecord which channel a row landed on, got: %s", body)
	}
}

// TestChannelViewPayload drives a real recorder through the usage entry point and
// reads the answer back through the /channel handler: the enabled flag, the window summary,
// the raw record behind it and the collector's health.
func TestChannelViewPayload(t *testing.T) {
	recorder, directory := startChannelRecorder(t)
	feedChannelRequest(t, recorder)

	request := pluginAPIRequest(BasePath + "/channel?window=24h")
	response := route(&request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /channel: status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if contentType := response.Headers.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("GET /channel: content-type = %q, want application/json", contentType)
	}

	envelope := decodeObject(t, response.Body, "the /channel payload")
	requireKeys(t, "the /channel payload", envelope, "enabled", "summary", "records", "records_total", "health")
	requireKeys(t, "summary", decodeObject(t, envelope["summary"], "summary"),
		"resolved_requests", "unresolved_requests", "off_baseline_requests", "baseline_provider",
		"providers", "cpa_providers", "hours")
	recordKeys := decodeObject(t, firstElement(t, envelope["records"], "records"), "records[0]")
	requireKeys(t, "records[0]", recordKeys, "cpa_provider", "ttft_ms")
	// No fact source is wired here, so the record has no gateway channel. The key must be
	// absent rather than empty: a page that reads final_provider as a channel has to be able
	// to tell "unknown" from "on the baseline".
	for _, absent := range []string{"final_provider", "resolved_provider", "gateway_provider", "channel_source"} {
		if _, present := recordKeys[absent]; present {
			t.Errorf("records[0] carries %s = %s with no fact source wired, want the key omitted",
				absent, recordKeys[absent])
		}
	}

	var payload struct {
		Enabled      bool                 `json:"enabled"`
		Summary      observation.Summary  `json:"summary"`
		Records      []observation.Record `json:"records"`
		RecordsTotal int                  `json:"records_total"`
		Health       observation.Health   `json:"health"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("the /channel payload must decode into the view shape: %v", errUnmarshal)
	}
	if !payload.Enabled {
		t.Error("enabled = false, want true while a recorder is published")
	}
	// The window has one record and no channel for it: the honest split is 0 resolved, 1
	// unresolved, and neither number is allowed to be a baseline hit.
	if payload.Summary.Resolved != 0 || payload.Summary.Unresolved != 1 {
		t.Errorf("resolved/unresolved = %d/%d, want 0/1 without a fact source",
			payload.Summary.Resolved, payload.Summary.Unresolved)
	}
	if len(payload.Summary.Providers) != 0 || len(payload.Summary.CPAProviders) != 1 {
		t.Errorf("providers = %+v with cpa_providers = %+v, want only the credential row",
			payload.Summary.Providers, payload.Summary.CPAProviders)
	}
	if payload.Summary.OffBaseline != 0 {
		t.Errorf("off_baseline_requests = %d, want 0: an unknown channel is not off baseline", payload.Summary.OffBaseline)
	}
	if payload.Summary.Baseline != channelFixtureBaseline {
		t.Errorf("baseline_provider = %q, want %q", payload.Summary.Baseline, channelFixtureBaseline)
	}
	if payload.Summary.Window != "24h" {
		t.Errorf("window = %q, want 24h", payload.Summary.Window)
	}
	// The timeline covers the window hour by hour, not only the hours that saw traffic: a
	// quiet hour has to be a gap, not a missing point that shifts its neighbours.
	hours := payload.Summary.Hours
	if len(hours) < 24 {
		t.Errorf("hours = %d points, want the whole 24h window covered", len(hours))
	}
	if len(hours) > 0 {
		first, last := hours[0].Hour, hours[len(hours)-1].Hour
		if want := payload.Summary.From.Truncate(time.Hour); !first.Equal(want) {
			t.Errorf("the timeline starts at %v, want %v", first, want)
		}
		if want := payload.Summary.To.Truncate(time.Hour); !last.Equal(want) {
			t.Errorf("the timeline ends at %v, want %v", last, want)
		}
	}
	if payload.RecordsTotal != 1 || len(payload.Records) != 1 {
		t.Fatalf("records_total = %d with %d records, want the one fed request", payload.RecordsTotal, len(payload.Records))
	}
	record := payload.Records[0]
	if record.RequestID != channelFixtureRequestID {
		t.Errorf("request_id = %q, want %q", record.RequestID, channelFixtureRequestID)
	}
	if record.CPProvider != channelFixtureBaseline {
		t.Errorf("cpa_provider = %q, want the credential %q the payload reported",
			record.CPProvider, channelFixtureBaseline)
	}
	if record.FinalProvider != "" || record.GatewayProvider != "" || record.ChannelSource != "" {
		t.Errorf("record = %+v, want no gateway channel: no fact source is wired into this recorder", record)
	}
	if record.TTFTMs != 0 {
		t.Errorf("ttft_ms = %d, want 0: the fixture payload reports no TTFT", record.TTFTMs)
	}
	if !payload.Health.Enabled || payload.Health.Directory != directory {
		t.Errorf("health = enabled %v in %q, want enabled true in %q", payload.Health.Enabled, payload.Health.Directory, directory)
	}
	if payload.Health.Events != 1 {
		t.Errorf("health events = %d, want the one fed request", payload.Health.Events)
	}
	// tokens_per_second is the one channel field this fixture cannot carry: Record.TPS is
	// `omitempty` on the record and is computed only from a decode window at or above
	// observation.minDecodeWindowMs, while the fixture reports zero timings. The contract is
	// pinned in both halves instead: a zero-speed record leaves the key out, which is the case
	// the page renders as "—", and a record that does carry a speed marshals it under the same
	// key the view reads (checked below).
	if _, present := recordKeys["tokens_per_second"]; present {
		t.Errorf("records[0] carries tokens_per_second = %s for a record with no measured decode time, want the key omitted",
			recordKeys["tokens_per_second"])
	}
	encoded, errMarshal := json.Marshal(observation.Record{
		CPProvider:      channelFixtureBaseline,
		FinalProvider:   channelFixtureGateway,
		GatewayProvider: channelFixtureGateway,
		ChannelSource:   observation.ChannelSourceLog,
		TTFTMs:          42,
		TPS:             1234.5,
	})
	if errMarshal != nil {
		t.Fatalf("a record must marshal: %v", errMarshal)
	}
	for _, key := range []string{
		"cpa_provider", "final_provider", "gateway_provider", "channel_source", "ttft_ms", "tokens_per_second",
	} {
		if !strings.Contains(string(encoded), `"`+key+`"`) {
			t.Errorf("the record the view marshals is missing the %s key: %s", key, encoded)
		}
	}
}

// TestChannelInvalidWindow asserts both endpoints reject a window they cannot answer with a
// JSON error code the page can branch on, instead of quietly serving the default one.
func TestChannelInvalidWindow(t *testing.T) {
	loadTestConfig(defaultConfigBytes())
	state.SetObservation(nil)
	for _, path := range []string{"/channel", "/channel.csv"} {
		request := pluginAPIRequest(BasePath + path + "?window=30d")
		response := route(&request)
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s?window=30d: status = %d, want %d", path, response.StatusCode, http.StatusBadRequest)
			continue
		}
		if contentType := response.Headers.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
			t.Errorf("GET %s?window=30d: content-type = %q, want application/json", path, contentType)
		}
		var payload struct {
			Error string `json:"error"`
		}
		if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
			t.Fatalf("GET %s?window=30d: the error must be JSON: %v", path, errUnmarshal)
		}
		if payload.Error != "invalid_window" {
			t.Errorf("GET %s?window=30d: error = %q, want invalid_window", path, payload.Error)
		}
	}
}

// TestChannelCSVExport asserts the export is a real attachment with the channel columns and
// one row per stored record. This recorder has no fact source, so the row carries the
// credential and empty channel columns: off_baseline has to be empty rather than "no", because
// the column marks deviation and this row has nothing to deviate from.
func TestChannelCSVExport(t *testing.T) {
	recorder, _ := startChannelRecorder(t)
	feedChannelRequest(t, recorder)

	request := pluginAPIRequest(BasePath + "/channel.csv?window=24h")
	response := route(&request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /channel.csv: status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if contentType := response.Headers.Get("Content-Type"); !strings.Contains(contentType, "text/csv") {
		t.Errorf("GET /channel.csv: content-type = %q, want text/csv", contentType)
	}
	if disposition := response.Headers.Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment") {
		t.Errorf("GET /channel.csv: content-disposition = %q, want an attachment", disposition)
	}

	rows, errRead := csv.NewReader(strings.NewReader(string(response.Body))).ReadAll()
	if errRead != nil {
		t.Fatalf("the export must parse as CSV: %v", errRead)
	}
	if len(rows) < 2 {
		t.Fatalf("the export has %d rows, want a header and the data row: %q", len(rows), response.Body)
	}
	header, row := rows[0], rows[1]
	if len(row) != len(header) {
		t.Fatalf("the data row has %d fields, the header has %d", len(row), len(header))
	}
	column := map[string]int{}
	for position, name := range header {
		column[name] = position
	}
	for _, name := range []string{
		"cpa_provider", "gateway_provider", "gateway_resolved_provider", "gateway_slug",
		"gateway_attempts", "gateway_cost", "channel_source",
		"pinned_provider", "canonical_slug", "off_baseline",
	} {
		if _, ok := column[name]; !ok {
			t.Fatalf("the export has no %s column: %v", name, header)
		}
	}
	for _, gone := range []string{"final_provider", "resolved_provider"} {
		if _, ok := column[gone]; ok {
			t.Errorf("the export still carries the %s column, which used to hold the credential", gone)
		}
	}
	if got := row[column["cpa_provider"]]; got != channelFixtureBaseline {
		t.Errorf("cpa_provider = %q, want %q", got, channelFixtureBaseline)
	}
	for _, name := range []string{"gateway_provider", "gateway_resolved_provider", "gateway_slug", "channel_source"} {
		if got := row[column[name]]; got != "" {
			t.Errorf("%s = %q, want it empty: this record has no fact", name, got)
		}
	}
	if got := row[column["off_baseline"]]; got != "" {
		t.Errorf("off_baseline = %q for a row with no known channel, want it empty", got)
	}
	if got, want := response.Headers.Get("X-Record-Count"), strconv.Itoa(len(rows)-1); got != want {
		t.Errorf("X-Record-Count = %q, want %q", got, want)
	}
}

// TestChannelViewJoinsTheGatewayChannel drives the whole path through the handlers: a usage
// payload the host reported, a fact the request-log scanner parsed, and the two dimensions
// they produce in the view and in the export.
func TestChannelViewJoinsTheGatewayChannel(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	recorder, _ := startChannelRecorderWithFacts(t, fixtureFacts{
		channelFixtureFact(at.Add(channelFixtureArrivalGap)),
	})
	// The record and the fact share the session uuid; only the fact knows the channel.
	feedChannelPayload(t, recorder, channelUsagePayloadAt("codex:session-"+channelFixtureSessionUUID, at))

	request := pluginAPIRequest(BasePath + "/channel?window=24h")
	response := route(&request)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /channel: status = %d", response.StatusCode)
	}
	envelope := decodeObject(t, response.Body, "the /channel payload")
	recordKeys := decodeObject(t, firstElement(t, envelope["records"], "records"), "records[0]")
	requireKeys(t, "records[0]", recordKeys,
		"cpa_provider", "final_provider", "resolved_provider", "gateway_provider",
		"gateway_resolved_provider", "gateway_slug", "gateway_attempts", "gateway_cost", "channel_source")
	for key, want := range map[string]string{
		"cpa_provider":              `"` + channelFixtureBaseline + `"`,
		"final_provider":            `"` + channelFixtureGateway + `"`,
		"resolved_provider":         `"` + channelFixtureGateway + `"`,
		"gateway_provider":          `"` + channelFixtureGateway + `"`,
		"gateway_resolved_provider": `"` + channelFixtureGateway + `"`,
		"gateway_slug":              `"` + channelFixtureGateway + `/deepseek-v4.1-flash"`,
		"gateway_attempts":          "1",
		"gateway_cost":              "0.00001515",
		"channel_source":            `"` + observation.ChannelSourceLog + `"`,
	} {
		if got := string(recordKeys[key]); got != want {
			t.Errorf("records[0].%s = %s, want %s", key, got, want)
		}
	}

	var payload struct {
		Summary observation.Summary `json:"summary"`
	}
	if errUnmarshal := json.Unmarshal(response.Body, &payload); errUnmarshal != nil {
		t.Fatalf("the /channel payload must decode into the view shape: %v", errUnmarshal)
	}
	summary := payload.Summary
	if summary.Resolved != 1 || summary.Unresolved != 0 {
		t.Errorf("resolved/unresolved = %d/%d, want 1/0 for a joined record", summary.Resolved, summary.Unresolved)
	}
	if summary.OffBaseline != 1 || summary.OffRatio != 1 {
		t.Errorf("off_baseline = %d (ratio %v), want 1 at 1: the real channel is %q, not the baseline",
			summary.OffBaseline, summary.OffRatio, channelFixtureGateway)
	}
	if len(summary.Providers) != 1 || summary.Providers[0].Provider != channelFixtureGateway {
		t.Fatalf("providers = %+v, want the real channel %q", summary.Providers, channelFixtureGateway)
	}
	if summary.Providers[0].OffBaseline != 1 {
		t.Errorf("the channel row = %+v, want it off the baseline", summary.Providers[0])
	}
	// The credential dimension is the one that was there before the channel log: same
	// request, the other half of it.
	if len(summary.CPAProviders) != 1 || summary.CPAProviders[0].Provider != channelFixtureBaseline {
		t.Fatalf("cpa_providers = %+v, want the credential %q", summary.CPAProviders, channelFixtureBaseline)
	}
	if summary.CPAProviders[0].OffBaseline != 0 {
		t.Errorf("the credential row = %+v, want off_baseline 0 in the credential dimension", summary.CPAProviders[0])
	}

	// The export tells the same story as the view: the credential under cpa_provider, the
	// channel under gateway_provider, and off_baseline decided on the channel.
	csvRequest := pluginAPIRequest(BasePath + "/channel.csv?window=24h")
	csvResponse := route(&csvRequest)
	rows, errRead := csv.NewReader(strings.NewReader(string(csvResponse.Body))).ReadAll()
	if errRead != nil {
		t.Fatalf("the export must parse as CSV: %v", errRead)
	}
	if len(rows) != 2 {
		t.Fatalf("the export has %d rows, want a header and one data row", len(rows))
	}
	column := map[string]int{}
	for position, name := range rows[0] {
		column[name] = position
	}
	for name, want := range map[string]string{
		"cpa_provider":              channelFixtureBaseline,
		"gateway_provider":          channelFixtureGateway,
		"gateway_resolved_provider": channelFixtureGateway,
		"gateway_slug":              channelFixtureGateway + "/deepseek-v4.1-flash",
		"gateway_attempts":          "1",
		"channel_source":            observation.ChannelSourceLog,
		"off_baseline":              "yes",
	} {
		if got := rows[1][column[name]]; got != want {
			t.Errorf("csv %s = %q, want %q", name, got, want)
		}
	}
}
