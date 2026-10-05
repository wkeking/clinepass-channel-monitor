package observation

import (
	"strings"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/channellog"
)

// This file holds the join: the half of a request's story the usage hook cannot tell.
//
// The usage payload names the CPA-side credential the request was served by
// (cpa_provider, e.g. "openai-compatible-cline1") and nothing about the Cline gateway channel
// behind it. That channel — finalProvider / resolvedProvider / canonicalSlug / the attempt
// counts / the gateway cost — is only in the upstream chat response, which CPA writes into
// its per-request debug log and drops when it translates the response for the client
// (internal/channellog). The recorder therefore reads the scanner's facts whenever a view is
// rendered and matches each record against them.
//
// The match rule is the one measured on live traffic, and it is deliberately narrow:
//
//   - the fact must carry a channel at all. A request that failed has no routing block, and an
//     empty channel must never be filled in from anything else: a guessed "deepseek" is
//     indistinguishable from a measured one.
//   - first, and for as long as it finds anything, the session: the fact's session uuid (the
//     "Session_id: session-<uuid>" header, prefix stripped) must appear inside the record's
//     session_id, which CPA writes as "codex:session-<uuid>". The 8 trailing characters of the
//     log file name are a CPA-internal id that appears nowhere in the usage payload, so they
//     are not a join key.
//   - |fact.time - record.time| must be shorter than the window the path uses (joinWindow for
//     the session match, the wider joinUsageWindow for the counter fallback). Measured on live
//     traffic the two clocks are 72-218 ms apart; the window is the allowance, not the
//     expectation.
//
// The current client traffic sends no Session_id header at all (every fact carries
// has_session false), so the session cannot match anything and the match falls back to the
// counters the two halves both report — see matchByUsage. A client that sends neither a header
// nor tokens still cannot be joined: those records keep an empty channel, which is the honest
// answer rather than a guessed one.
type FactSource interface {
	Facts() []channellog.Fact
}

// FactSourceFunc adapts a lookup to a FactSource. The lookup is called on every read, never
// captured: the plugin starts the recorder and the request-log scanner in one reconfigure, and
// a scanner that is published a moment later must still be picked up.
type FactSourceFunc func() []channellog.Fact

// Facts returns the source's newest facts, or nil when there is no source at all.
func (f FactSourceFunc) Facts() []channellog.Fact {
	if f == nil {
		return nil
	}
	return f()
}

// FactsFromChannelLog adapts the published CPA request-log scanner to a FactSource. It is the
// wiring the plugin passes to New, and it is written this way (rather than taking the scanner
// itself) because the two are published in sequence: the lookup resolves the running scanner at
// read time, so a reconfigure that starts them in either order works.
func FactsFromChannelLog(lookup func() *channellog.Scanner) FactSource {
	return FactSourceFunc(func() []channellog.Fact {
		scanner := lookup()
		if scanner == nil {
			return nil
		}
		return scanner.Facts()
	})
}

// ChannelSourceLog is what channel_source carries on a record that was joined. An empty value
// means the opposite: this build has no gateway channel for the record, and the page and the
// export must render it as "unknown", never as the baseline.
const ChannelSourceLog = "log"

// joinWindow is the widest |fact.time - record.time| the session match may be made on. The
// measured distance between the log's arrival timestamp and the usage-reported time is 72-218 ms
// for ordinary traffic, which is why two seconds is an allowance with an order of magnitude of
// headroom rather than a tight fit. The session match is exclusive — a fact exactly two seconds
// away belongs to some other request — because it picks the closest qualifying fact of a whole
// session, so a wide window would let one turn's channel drift onto the next.
const joinWindow = 2 * time.Second

// joinUsageWindow is the same allowance for the token-and-counter fallback, and it is wider than
// joinWindow on purpose: the two timestamps that fallback compares are further apart exactly when
// the request body is large. CPA stamps the request log's Timestamp when the request arrives,
// while the usage hook's RequestedAt is the later moment the body has been read; for the ~93 MB
// /v1/responses bodies seen on production (one client re-sends its whole context) the gap is
// 1.99-2.38 s, and for a one-megabyte body it is 14-218 ms. A two-second window therefore threw
// away the channel of every large-body request that landed past the boundary — the page showed
// "无渠道块" and the account guard never saw the sample — while five seconds covers the measured
// maximum with headroom.
//
// Widening this side costs little because the counters, not the clock, are what identify the
// request here, and the fallback still refuses an ambiguous pair rather than guessing. Across the
// 8,320 facts on production, no two facts share a counter pair within two seconds, three pairs do
// within five seconds (all of them the same 1,494-token probe request repeated), and an ambiguous
// pair is dropped instead of attached to the wrong request. This path admits the boundary itself,
// because there the window only has to exclude the impossible.
const joinUsageWindow = 5 * time.Second

// channelJoin is one read of the facts, held for the length of one view. It is rebuilt per
// query instead of being cached: a fact reaches the scanner seconds after the request it
// describes, and a list cached across queries would leave the newest records unjoined.
type channelJoin struct {
	facts []channellog.Fact
}

// openJoin reads the facts once. A recorder with no source, or one whose scanner is switched
// off, produces an empty join: every record then stays unjoined, which is the same answer as a
// deployment whose channel log is empty.
func (r *Recorder) openJoin() channelJoin {
	if r == nil || r.facts == nil {
		return channelJoin{}
	}
	return channelJoin{facts: r.facts.Facts()}
}

// match returns the fact that belongs to the record: the session match first, because it is
// proven, and the token-and-time fallback only when the session found nothing.
func (j channelJoin) match(record Record) (channellog.Fact, bool) {
	if fact, found := j.matchBySession(record); found {
		return fact, true
	}
	return j.matchByUsage(record)
}

// matchBySession is the proven path. When several facts qualify the closest one in time wins:
// the session header is what makes the match possible, and the timestamp is what picks among
// the requests of one session.
func (j channelJoin) matchBySession(record Record) (channellog.Fact, bool) {
	var best channellog.Fact
	found := false
	bestDelta := time.Duration(0)
	for _, fact := range j.facts {
		if !fact.HasChannel() || !fact.HasSession || fact.SessionUUID == "" {
			// A failed request, or a client that sent no session header: not a candidate.
			continue
		}
		if record.SessionID == "" || !strings.Contains(record.SessionID, fact.SessionUUID) {
			continue
		}
		delta := record.Time.Sub(fact.Time)
		if delta < 0 {
			delta = -delta
		}
		if delta >= joinWindow {
			continue
		}
		if !found || delta < bestDelta {
			best, bestDelta, found = fact, delta, true
		}
	}
	return best, found
}

// matchByUsage is the fallback for the traffic that sends no Session_id header: the usage
// callback and the request log both report the same token counters, and those counters are what
// identifies the request when nothing else can. The window is joinUsageWindow, the wider
// allowance, because a large request body pushes the two timestamps seconds apart (see the
// constant).
//
// It is a deliberately tight rule, and its seams are the point:
//
//   - the pair must be exact. A record of (37, 16) and a fact of (37, 17) are two different
//     requests, however close in time they are.
//   - a 0/0 on either side carries no information at all, so two zeros may never pair up: a
//     failed request reports no tokens, and a log that named no usage has no counters to match.
//     Both are skipped rather than being allowed to match each other.
//   - exactly one candidate, or no join at all. Two facts with the same counters in the same
//     window is a real possibility on a busy deployment, and picking one of them would attach a
//     measured channel to the wrong request. Ambiguity is not evidence.
func (j channelJoin) matchByUsage(record Record) (channellog.Fact, bool) {
	if record.InputTokens == 0 && record.OutputTokens == 0 {
		return channellog.Fact{}, false
	}
	var candidate channellog.Fact
	candidates := 0
	for _, fact := range j.facts {
		if !fact.HasChannel() {
			// A fact without a channel is never a candidate: it cannot fill one in.
			continue
		}
		if fact.PromptTokens == 0 && fact.CompletionTokens == 0 {
			continue
		}
		if fact.PromptTokens != record.InputTokens || fact.CompletionTokens != record.OutputTokens {
			continue
		}
		delta := record.Time.Sub(fact.Time)
		if delta < 0 {
			delta = -delta
		}
		if delta > joinUsageWindow {
			continue
		}
		candidate = fact
		candidates++
	}
	if candidates != 1 {
		return channellog.Fact{}, false
	}
	return candidate, true
}

// withChannel returns the record as the view reads it: a copy whose gateway half is filled from
// the fact. The stored record is never touched — the join is a read-time answer, and the JSONL
// line stays what the usage hook reported.
//
// final_provider and resolved_provider carry the gateway values under the names the page and
// the export have always used; the gateway_* keys say the same thing with the name this build
// prefers. Both are set from one fact, so they can never disagree.
func (record Record) withChannel(fact channellog.Fact) Record {
	record.FinalProvider = fact.FinalProvider
	record.ResolvedProvider = fact.ResolvedProvider
	record.GatewayProvider = fact.FinalProvider
	record.GatewayResolvedProvider = fact.ResolvedProvider
	record.GatewaySlug = fact.CanonicalSlug
	record.GatewayAttempts = fact.TotalProviderAttemptCount
	record.GatewayCost = fact.GatewayCost
	record.ChannelSource = ChannelSourceLog
	return record
}

// GatewayChannel is the real channel the record landed on, and the value "off baseline" is
// decided on. Empty means this build has no channel for the record: a request that failed, one
// whose client sent no session header, or one whose fact had not been parsed yet. None of
// those is a baseline hit.
func (record Record) GatewayChannel() string {
	return firstNonEmpty(record.GatewayProvider, record.FinalProvider,
		record.GatewayResolvedProvider, record.ResolvedProvider)
}
