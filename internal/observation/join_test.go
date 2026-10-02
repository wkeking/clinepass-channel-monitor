package observation

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/channellog"
)

// The fixtures below are one request seen from both sides: the usage record the host reports
// and the fact the CPA request-log scanner parsed. They deliberately disagree — the record
// carries the CPA credential, the fact carries the Cline gateway channel — because that
// disagreement is the whole point of the join. The spacing between the two timestamps is the
// one measured on live traffic (72-218 ms).
const (
	// joinSessionUUID is the uuid the client sent in its Session_id header. CPA writes the
	// record's session as "codex:session-<uuid>" and the log carries the bare header value.
	joinSessionUUID = "3f2a1b0c9d8e"
	// joinRecordSession is the session the usage record shows for that request.
	joinRecordSession = "codex:session-" + joinSessionUUID
	// joinOtherSessionUUID belongs to a different request of the same deployment.
	joinOtherSessionUUID = "9c1d2e3f4a5b"
	// joinGatewayChannel is the real channel the fixture fact names: the baseline one.
	joinGatewayChannel = providerBaseline
	// joinGatewayOther is a second real channel, i.e. one that is off the baseline.
	joinGatewayOther = "moonshot"
	// joinGatewayCost is the gateway's own cost figure for the fixture request.
	joinGatewayCost = 0.00001515
	// joinArrivalGap is how far the log's arrival timestamp sits from the usage-reported time.
	joinArrivalGap = 120 * time.Millisecond
)

// factSet is a FactSource a test fills after the recorder has started. The real scanner
// delivers a fact seconds after the record it belongs to, so a test has to be able to add
// facts between two reads — and a recorder that cached the list would fail here.
type factSet struct {
	mu    sync.Mutex
	facts []channellog.Fact
}

func (s *factSet) set(facts ...channellog.Fact) {
	s.mu.Lock()
	s.facts = append([]channellog.Fact(nil), facts...)
	s.mu.Unlock()
}

func (s *factSet) Facts() []channellog.Fact {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]channellog.Fact(nil), s.facts...)
}

// newRecorderWithFacts builds a recorder over a temporary store with the given fact source.
// A nil source is the deployment with the request-log scanner switched off.
func newRecorderWithFacts(t *testing.T, baseline string, source FactSource) (*Recorder, *testClock) {
	t.Helper()
	clock := &testClock{at: fixtureNow}
	recorder := New(Options{
		Enabled:       true,
		Directory:     t.TempDir(),
		RetentionDays: 3,
		MaxSizeMB:     16,
		Baseline:      baseline,
		Facts:         source,
	})
	recorder.nowFn = clock.now
	recorder.Start()
	t.Cleanup(recorder.Stop)
	return recorder, clock
}

// gatewayFact is the parsed request log of one successful request: the channel it was served
// on plus the two keys the join is made on.
func gatewayFact(at time.Time, sessionUUID, channel string) channellog.Fact {
	return channellog.Fact{
		Time:                      at,
		Path:                      "/v1/responses",
		Method:                    "POST",
		SessionID:                 "session-" + sessionUUID,
		SessionUUID:               sessionUUID,
		HasSession:                true,
		FinalProvider:             channel,
		ResolvedProvider:          channel,
		CanonicalSlug:             channel + "/deepseek-v4.1-flash",
		OriginalModelID:           channel + "/deepseek-v4.1-flash",
		PinnedProvider:            channel,
		AffinityOutcome:           "confirmed",
		ModelAttemptCount:         1,
		TotalProviderAttemptCount: 1,
		GatewayCost:               joinGatewayCost,
		Frames:                    5,
		AttemptsSeen:              1,
		SourceFile:                "v1_responses-fixture.log",
		ParsedAt:                  at,
	}
}

// failedFact is the request log of a request that never got a response with a routing block:
// the upstream answered with an error, so there is no channel in it. It carries the session
// and the arrival time of the record — everything except the channel.
func failedFact(at time.Time) channellog.Fact {
	fact := gatewayFact(at, joinSessionUUID, "")
	return channellog.Fact{
		Time:             fact.Time,
		Path:             fact.Path,
		Method:           fact.Method,
		SessionID:        fact.SessionID,
		SessionUUID:      fact.SessionUUID,
		HasSession:       fact.HasSession,
		Frames:           1,
		AttemptsSeen:     1,
		HadErrorResponse: true,
		SourceFile:       fact.SourceFile,
		ParsedAt:         fact.ParsedAt,
	}
}

// stampUsage pins the payload's own timestamp to the instant the test wants the record to
// carry, to the nanosecond: the join window is measured against it.
func stampUsage(wire *usageWire, at time.Time) {
	wire.RequestedAt = at.UTC().Format(time.RFC3339Nano)
}

// channelRecordsOf reads the window the way the 「渠道」 view does, with the gateway channel
// joined in.
func channelRecordsOf(t *testing.T, recorder *Recorder) []Record {
	t.Helper()
	recorder.Flush()
	records, errLoad := recorder.ChannelRecordsSince(mustWindow(t, "24h"))
	if errLoad != nil {
		t.Fatalf("ChannelRecordsSince: %v", errLoad)
	}
	return records
}

// TestJoinFillsTheGatewayChannel is the join itself: one record, one fact of the same session
// 120 ms later, and the two dimensions kept apart afterwards.
func TestJoinFillsTheGatewayChannel(t *testing.T) {
	source := &factSet{}
	recorder, clock := newRecorderWithFacts(t, providerBaseline, source)
	at := clock.at.Add(-time.Minute)
	wire := usageFixture(providerFixture, at)
	wire.SessionID = joinRecordSession
	stampUsage(&wire, at)
	feedUsage(t, recorder, wire)
	// The fact arrives after the record: the scanner only reads a log file once CPA has
	// stopped writing it.
	source.set(gatewayFact(at.Add(joinArrivalGap), joinSessionUUID, joinGatewayOther))

	records := channelRecordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want exactly 1 record, got %d: %+v", len(records), records)
	}
	record := records[0]
	for _, check := range []struct {
		name string
		got  any
		want any
	}{
		{"cpa_provider", record.CPProvider, providerFixture},
		{"final_provider", record.FinalProvider, joinGatewayOther},
		{"resolved_provider", record.ResolvedProvider, joinGatewayOther},
		{"gateway_provider", record.GatewayProvider, joinGatewayOther},
		{"gateway_resolved_provider", record.GatewayResolvedProvider, joinGatewayOther},
		{"gateway_slug", record.GatewaySlug, joinGatewayOther + "/deepseek-v4.1-flash"},
		{"gateway_attempts", record.GatewayAttempts, 1},
		{"gateway_cost", record.GatewayCost, joinGatewayCost},
		{"channel_source", record.ChannelSource, ChannelSourceLog},
	} {
		if check.got != check.want {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}

	// The store keeps what the usage hook said and nothing else: the gateway half is a
	// read-time answer, so a stored line can never be mistaken for a measured channel.
	raw := storedLine(t, recorder, wire.RequestID)
	if !strings.Contains(raw, `"cpa_provider":"`+providerFixture+`"`) {
		t.Errorf("the stored line does not carry the credential under cpa_provider: %s", raw)
	}
	for _, gone := range []string{"final_provider", "resolved_provider", "gateway_provider", "gateway_slug", "gateway_cost", "channel_source"} {
		if strings.Contains(raw, `"`+gone+`"`) {
			t.Errorf("the stored line carries the read-time key %q: %s", gone, raw)
		}
	}

	// The summary answers the two questions in two dimensions.
	summary := recorder.Summary(mustWindow(t, "24h"))
	if summary.Resolved != 1 || summary.Unresolved != 0 {
		t.Errorf("resolved/unresolved = %d/%d, want 1/0 for a joined record", summary.Resolved, summary.Unresolved)
	}
	if summary.OffBaseline != 1 || summary.OffRatio != 1 {
		t.Errorf("off_baseline = %d (ratio %v), want 1 at 1: the real channel is %q, not the baseline",
			summary.OffBaseline, summary.OffRatio, joinGatewayOther)
	}
	if summary.Channels != 1 {
		t.Errorf("channels = %d, want 1", summary.Channels)
	}
	if len(summary.Providers) != 1 || summary.Providers[0].Provider != joinGatewayOther {
		t.Fatalf("providers = %+v, want the real channel %q", summary.Providers, joinGatewayOther)
	}
	if summary.Providers[0].OffBaseline != 1 || summary.Providers[0].Ratio != 1 {
		t.Errorf("the channel row = %+v, want it off the baseline at ratio 1", summary.Providers[0])
	}
	if len(summary.CPAProviders) != 1 || summary.CPAProviders[0].Provider != providerFixture {
		t.Fatalf("cpa_providers = %+v, want the credential %q", summary.CPAProviders, providerFixture)
	}
	// The credential dimension never answers the off-baseline question, even when the
	// credential happens to be named like the baseline.
	if summary.CPAProviders[0].OffBaseline != 0 {
		t.Errorf("the credential row = %+v, want off_baseline 0 in the credential dimension", summary.CPAProviders[0])
	}
}

// TestJoinWindowIsExclusiveAtTheBoundary pins the match rule's timing half: the record and the
// fact have to be closer than two seconds, and two seconds exactly is already another request.
func TestJoinWindowIsExclusiveAtTheBoundary(t *testing.T) {
	for _, check := range []struct {
		name   string
		delta  time.Duration
		joined bool
	}{
		{"one millisecond inside the window", joinWindow - time.Millisecond, true},
		{"one millisecond before the record", -joinArrivalGap, true},
		{"exactly at the window", joinWindow, false},
		{"exactly the window before the record", -joinWindow, false},
	} {
		t.Run(check.name, func(t *testing.T) {
			source := &factSet{}
			recorder, clock := newRecorderWithFacts(t, providerBaseline, source)
			at := clock.at.Add(-time.Minute)
			wire := usageFixture(providerFixture, at)
			wire.SessionID = joinRecordSession
			stampUsage(&wire, at)
			feedUsage(t, recorder, wire)
			source.set(gatewayFact(at.Add(check.delta), joinSessionUUID, joinGatewayChannel))

			records := channelRecordsOf(t, recorder)
			if len(records) != 1 {
				t.Fatalf("want exactly 1 record, got %d", len(records))
			}
			record := records[0]
			if check.joined {
				if record.ChannelSource != ChannelSourceLog || record.GatewayProvider != joinGatewayChannel {
					t.Errorf("record = %+v, want it joined on %q at a delta of %v",
						record, joinGatewayChannel, check.delta)
				}
			} else {
				if record.ChannelSource != "" || record.GatewayProvider != "" || record.FinalProvider != "" {
					t.Errorf("record = %+v, want no channel at a delta of %v", record, check.delta)
				}
			}
			summary := recorder.Summary(mustWindow(t, "24h"))
			if check.joined && (summary.Resolved != 1 || summary.Unresolved != 0) {
				t.Errorf("resolved/unresolved = %d/%d, want 1/0", summary.Resolved, summary.Unresolved)
			}
			if !check.joined && (summary.Resolved != 0 || summary.Unresolved != 1) {
				t.Errorf("resolved/unresolved = %d/%d, want 0/1", summary.Resolved, summary.Unresolved)
			}
		})
	}
}

// TestJoinNeedsTheSessionHeader keeps the fallback out: without a session the two halves
// cannot be told apart, so the record stays unjoined rather than being guessed onto a fact.
func TestJoinNeedsTheSessionHeader(t *testing.T) {
	for _, check := range []struct {
		name string
		fact channellog.Fact
		wire func(*usageWire)
	}{
		{
			name: "a fact of another session",
			fact: gatewayFact(fixtureNow.Add(-time.Minute+joinArrivalGap), joinOtherSessionUUID, joinGatewayOther),
			wire: func(wire *usageWire) { wire.SessionID = joinRecordSession },
		},
		{
			name: "a fact whose client sent no session header",
			fact: func() channellog.Fact {
				fact := gatewayFact(fixtureNow.Add(-time.Minute+joinArrivalGap), "", joinGatewayOther)
				fact.SessionID = ""
				fact.HasSession = false
				return fact
			}(),
			wire: func(wire *usageWire) { wire.SessionID = joinRecordSession },
		},
		{
			name: "a record whose client sent no session header",
			fact: gatewayFact(fixtureNow.Add(-time.Minute+joinArrivalGap), joinSessionUUID, joinGatewayOther),
			wire: func(wire *usageWire) { wire.SessionID = "lcp:v1:deadbeef" },
		},
	} {
		t.Run(check.name, func(t *testing.T) {
			source := &factSet{}
			recorder, clock := newRecorderWithFacts(t, providerBaseline, source)
			at := clock.at.Add(-time.Minute)
			wire := usageFixture(providerFixture, at)
			check.wire(&wire)
			stampUsage(&wire, at)
			feedUsage(t, recorder, wire)
			source.set(check.fact)

			records := channelRecordsOf(t, recorder)
			if len(records) != 1 {
				t.Fatalf("want exactly 1 record, got %d", len(records))
			}
			if got := records[0]; got.ChannelSource != "" || got.GatewayProvider != "" || got.FinalProvider != "" {
				t.Errorf("record = %+v, want an empty channel: the session is what the match rests on", got)
			}
			summary := recorder.Summary(mustWindow(t, "24h"))
			if summary.Resolved != 0 || summary.Unresolved != 1 {
				t.Errorf("resolved/unresolved = %d/%d, want 0/1", summary.Resolved, summary.Unresolved)
			}
			if len(summary.Providers) != 0 {
				t.Errorf("providers = %+v, want no channel row for an unjoined record", summary.Providers)
			}
			if len(summary.CPAProviders) != 1 || summary.CPAProviders[0].Provider != providerFixture {
				t.Errorf("cpa_providers = %+v, want the credential the record did report", summary.CPAProviders)
			}
		})
	}
}

// TestJoinNeverTakesAChannelFromAFailedRequest is the honest-empty rule: the fact of a request
// that failed has no routing block, and matching it must not invent a channel — least of all
// the baseline one.
func TestJoinNeverTakesAChannelFromAFailedRequest(t *testing.T) {
	source := &factSet{}
	recorder, clock := newRecorderWithFacts(t, providerBaseline, source)
	at := clock.at.Add(-time.Minute)
	wire := usageFixture(providerFixture, at)
	wire.SessionID = joinRecordSession
	stampUsage(&wire, at)
	feedUsage(t, recorder, wire)
	fact := failedFact(at.Add(joinArrivalGap))
	if fact.HasChannel() {
		t.Fatalf("the fixture failed fact carries a channel: %+v", fact)
	}
	source.set(fact)

	records := channelRecordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want exactly 1 record, got %d", len(records))
	}
	record := records[0]
	if record.ChannelSource != "" || record.GatewayProvider != "" || record.FinalProvider != "" || record.ResolvedProvider != "" {
		t.Errorf("record = %+v, want an empty channel for a failed request", record)
	}
	if record.CPProvider != providerFixture {
		t.Errorf("cpa_provider = %q, want the credential to survive: %q", record.CPProvider, providerFixture)
	}

	summary := recorder.Summary(mustWindow(t, "24h"))
	if summary.Resolved != 0 || summary.Unresolved != 1 {
		t.Errorf("resolved/unresolved = %d/%d, want 0/1: a failure cannot be attributed to a channel",
			summary.Resolved, summary.Unresolved)
	}
	if len(summary.Providers) != 0 {
		t.Errorf("providers = %+v, want no channel row for a failure", summary.Providers)
	}
	if summary.OffBaseline != 0 || summary.OffRatio != 0 {
		t.Errorf("off_baseline = %d (ratio %v), want 0: an unknown channel is not a baseline hit",
			summary.OffBaseline, summary.OffRatio)
	}
	if len(summary.CPAProviders) != 1 || summary.CPAProviders[0].Provider != providerFixture {
		t.Errorf("cpa_providers = %+v, want the credential of the failed request", summary.CPAProviders)
	}

	// The export keeps the same distinction: the credential is filled, the channel is not.
	header, rows := csvExport(t, recorder)
	if len(rows) != 1 {
		t.Fatalf("want 1 exported row, got %d: %v", len(rows), rows)
	}
	columns := csvColumnIndex(header)
	if got := rows[0][columns["cpa_provider"]]; got != providerFixture {
		t.Errorf("csv cpa_provider = %q, want %q", got, providerFixture)
	}
	for _, name := range []string{"gateway_provider", "gateway_resolved_provider", "gateway_slug", "channel_source", "off_baseline"} {
		if got := rows[0][columns[name]]; got != "" {
			t.Errorf("csv %s = %q, want it empty for a request with no known channel", name, got)
		}
	}
}

// TestSummarySplitsTheChannelFromTheCredential drives three requests through one recorder: two
// of them joined onto different real channels under two different credentials, and one whose
// client sent no session header, which can never be joined. The two dimensions have to tell
// that story without either of them borrowing the other's numbers.
func TestSummarySplitsTheChannelFromTheCredential(t *testing.T) {
	source := &factSet{}
	recorder, clock := newRecorderWithFacts(t, providerBaseline, source)
	base := clock.at.Add(-time.Hour)

	onBaseline := usageFixture(providerFixture, base)
	onBaseline.RequestID = "req-joined-baseline"
	onBaseline.SessionID = joinRecordSession
	stampUsage(&onBaseline, base)

	offBaseline := usageFixture("openai-compatible-cline2", base.Add(time.Minute))
	offBaseline.RequestID = "req-joined-off-baseline"
	offBaseline.SessionID = "codex:session-" + joinOtherSessionUUID
	stampUsage(&offBaseline, base.Add(time.Minute))

	noSession := usageFixture("openai-compatible-cline3", base.Add(2*time.Minute))
	noSession.RequestID = "req-without-session"
	noSession.SessionID = "lcp:v1:0f0e0d0c"
	stampUsage(&noSession, base.Add(2*time.Minute))

	feedUsage(t, recorder, onBaseline)
	feedUsage(t, recorder, offBaseline)
	feedUsage(t, recorder, noSession)
	source.set(
		gatewayFact(base.Add(joinArrivalGap), joinSessionUUID, joinGatewayChannel),
		gatewayFact(base.Add(time.Minute+joinArrivalGap), joinOtherSessionUUID, joinGatewayOther),
	)

	summary := recorder.Summary(mustWindow(t, "24h"))
	if summary.Resolved != 2 || summary.Unresolved != 1 {
		t.Fatalf("resolved/unresolved = %d/%d, want 2/1", summary.Resolved, summary.Unresolved)
	}
	if summary.OffBaseline != 1 || summary.OffRatio != 0.5 {
		t.Errorf("off_baseline = %d (ratio %v), want 1 at 0.5 over the joined records",
			summary.OffBaseline, summary.OffRatio)
	}
	if summary.Channels != 2 {
		t.Errorf("channels = %d, want the two real channels", summary.Channels)
	}

	rows := map[string]ProviderStat{}
	for _, row := range summary.Providers {
		rows[row.Provider] = row
	}
	if len(rows) != 2 {
		t.Fatalf("providers = %+v, want one row per real channel", summary.Providers)
	}
	if got := rows[joinGatewayChannel]; got.Requests != 1 || got.OffBaseline != 0 || got.Ratio != 0.5 {
		t.Errorf("the %s channel row = %+v, want 1 request on the baseline at ratio 0.5", joinGatewayChannel, got)
	}
	if got := rows[joinGatewayOther]; got.Requests != 1 || got.OffBaseline != 1 || got.Ratio != 0.5 {
		t.Errorf("the %s channel row = %+v, want 1 request off the baseline at ratio 0.5", joinGatewayOther, got)
	}

	credentials := map[string]ProviderStat{}
	for _, row := range summary.CPAProviders {
		credentials[row.Provider] = row
	}
	if len(credentials) != 3 {
		t.Fatalf("cpa_providers = %+v, want one row per credential", summary.CPAProviders)
	}
	for name, want := range map[string]int64{providerFixture: 1, "openai-compatible-cline2": 1, "openai-compatible-cline3": 1} {
		got, ok := credentials[name]
		if !ok {
			t.Errorf("cpa_providers has no row for %q: %+v", name, summary.CPAProviders)
			continue
		}
		if got.Requests != want {
			t.Errorf("the %s credential row = %+v, want %d request(s)", name, got, want)
		}
		if got.OffBaseline != 0 {
			t.Errorf("the %s credential row = %+v, want off_baseline 0 in the credential dimension", name, got)
		}
	}
	// The credential dimension counts the unjoined request too: the credential is a fact of
	// the usage hook whatever the channel log knows.
	if got := credentials["openai-compatible-cline3"]; got.Ratio <= 0 {
		t.Errorf("the credential row = %+v, want it counted with a ratio", got)
	}
}

// TestSummaryFlipsWithTheBaseline pins that off-baseline is a property of the question, not of
// the record: the same two joined requests give the opposite answer under the other baseline.
// The credential dimension is not asked the question at all.
func TestSummaryFlipsWithTheBaseline(t *testing.T) {
	source := &factSet{}
	flipped, clock := newRecorderWithFacts(t, joinGatewayOther, source)
	base := clock.at.Add(-time.Hour)

	first := usageFixture(providerFixture, base)
	first.RequestID = "req-flip-a"
	first.SessionID = joinRecordSession
	stampUsage(&first, base)
	second := usageFixture(providerFixture, base.Add(time.Minute))
	second.RequestID = "req-flip-b"
	second.SessionID = "codex:session-" + joinOtherSessionUUID
	stampUsage(&second, base.Add(time.Minute))
	feedUsage(t, flipped, first)
	feedUsage(t, flipped, second)
	source.set(
		gatewayFact(base.Add(joinArrivalGap), joinSessionUUID, joinGatewayChannel),
		gatewayFact(base.Add(time.Minute+joinArrivalGap), joinOtherSessionUUID, joinGatewayOther),
	)

	summary := flipped.Summary(mustWindow(t, "24h"))
	if summary.Resolved != 2 || summary.OffBaseline != 1 {
		t.Fatalf("summary resolved=%d off_baseline=%d, want 2/1", summary.Resolved, summary.OffBaseline)
	}
	rows := map[string]ProviderStat{}
	for _, row := range summary.Providers {
		rows[row.Provider] = row
	}
	if got := rows[joinGatewayChannel]; got.OffBaseline != 1 {
		t.Errorf("%s row = %+v, want it off the flipped baseline", joinGatewayChannel, got)
	}
	if got := rows[joinGatewayOther]; got.OffBaseline != 0 {
		t.Errorf("%s row = %+v, want it on the flipped baseline", joinGatewayOther, got)
	}
	for _, row := range summary.CPAProviders {
		if row.OffBaseline != 0 {
			t.Errorf("credential row %+v is off baseline, want the credential dimension never asked", row)
		}
	}
}

// TestLegacyV2LineReadsAsTheCPACredential keeps an old window working. A v2 line named the
// credential final_provider / resolved_provider; reading it as a channel would file a
// credential as a channel and make an old window look joined.
func TestLegacyV2LineReadsAsTheCPACredential(t *testing.T) {
	directory := t.TempDir()
	now := time.Now().UTC()
	line := `{"v":2,"time":"` + now.Add(-time.Hour).UTC().Format(time.RFC3339) + `","request_id":"legacy-v2",` +
		`"session_id":"codex:session-legacy","model":"deepseek-flash-1",` +
		`"upstream_model":"deepseek/deepseek-v4.1-flash","canonical_slug":"deepseek/deepseek-v4.1-flash",` +
		`"final_provider":"` + providerFixture + `","resolved_provider":"` + providerFixture + `",` +
		`"auth_id":"openai-compatibility:cline1:legacy","auth_type":"apikey",` +
		`"stream":true,"status_code":200,"ttft_ms":900,"duration_ms":2000,"decode_ms":1100,` +
		`"input_tokens":800,"output_tokens":200,"total_tokens":1000}` + "\n"
	writeStoredLine(t, directory, now, line)

	recorder := New(Options{Enabled: true, Directory: directory, RetentionDays: 3, MaxSizeMB: 16, Baseline: providerBaseline})
	recorder.Start()
	t.Cleanup(recorder.Stop)

	records := channelRecordsOf(t, recorder)
	if len(records) != 1 {
		t.Fatalf("want the v2 line back, got %d records", len(records))
	}
	record := records[0]
	if record.Schema != 2 || record.CPProvider != providerFixture {
		t.Errorf("record = %+v, want the v2 line read as cpa_provider %q", record, providerFixture)
	}
	if record.FinalProvider != "" || record.ResolvedProvider != "" || record.GatewayProvider != "" ||
		record.GatewaySlug != "" || record.ChannelSource != "" {
		t.Errorf("record = %+v, want an empty gateway half: a v2 line cannot carry a channel", record)
	}
	if record.AuthID != "openai-compatibility:cline1:legacy" {
		t.Errorf("record = %+v, want the credential identity preserved", record)
	}

	summary := recorder.Summary(mustWindow(t, "24h"))
	if summary.Resolved != 0 || summary.Unresolved != 1 {
		t.Errorf("resolved/unresolved = %d/%d, want 0/1: an old line is not a joined one",
			summary.Resolved, summary.Unresolved)
	}
	if len(summary.Providers) != 0 {
		t.Errorf("providers = %+v, want no channel row for a v2 line", summary.Providers)
	}
	if len(summary.CPAProviders) != 1 || summary.CPAProviders[0].Provider != providerFixture {
		t.Errorf("cpa_providers = %+v, want the legacy credential under cpa_provider", summary.CPAProviders)
	}

	// The export labels it honestly: the credential column is filled, the channel columns are
	// empty, so nothing in the file reads as "this request was on the baseline".
	header, rows := csvExport(t, recorder)
	if len(rows) != 1 {
		t.Fatalf("want 1 exported row, got %d", len(rows))
	}
	columns := csvColumnIndex(header)
	if got := rows[0][columns["cpa_provider"]]; got != providerFixture {
		t.Errorf("csv cpa_provider = %q, want %q", got, providerFixture)
	}
	for _, name := range []string{"gateway_provider", "gateway_resolved_provider", "gateway_slug", "channel_source", "off_baseline"} {
		if got := rows[0][columns[name]]; got != "" {
			t.Errorf("csv %s = %q, want it empty on a v2 line", name, got)
		}
	}
	// The two numeric channel columns read zero rather than empty, which is all a numeric
	// column can say; the empty channel_source is what marks the row as carrying no channel.
	for name, want := range map[string]string{"gateway_attempts": "0", "gateway_cost": "0.00000000"} {
		if got := rows[0][columns[name]]; got != want {
			t.Errorf("csv %s = %q, want %q on a v2 line", name, got, want)
		}
	}
}
