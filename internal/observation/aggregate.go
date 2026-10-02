package observation

import (
	"sort"
	"strings"
	"time"
)

// Window is the reporting range of the channel view.
type Window string

const (
	Window1h  Window = "1h"
	Window24h Window = "24h"
	Window7d  Window = "7d"
)

// ParseWindow maps a query value to a window. The empty value is the default window, which
// is the one the operator question is asked in: the last day.
func ParseWindow(raw string) (Window, bool) {
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "":
		return Window24h, true
	case string(Window1h):
		return Window1h, true
	case string(Window24h):
		return Window24h, true
	case string(Window7d):
		return Window7d, true
	default:
		return Window24h, false
	}
}

// Duration returns the length of the window.
func (w Window) Duration() time.Duration {
	switch w {
	case Window1h:
		return time.Hour
	case Window7d:
		return 7 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// ProviderStat is one row of a provider dimension.
//
// The same shape serves both dimensions, so the two tables on the page and in the payload read
// the same way; what a row means depends on which list it is in. See Summary.
type ProviderStat struct {
	Provider string `json:"provider"`
	Requests int64  `json:"requests"`
	// Ratio is the row's share of its own dimension (the real channels, or the credentials).
	Ratio float64 `json:"ratio"`
	// OffBaseline counts the row's requests whose real channel differs from the baseline. It
	// is always zero in the credential dimension: a CPA credential does not answer the
	// question "was this the official channel", and mixing the two is the mistake this
	// rename exists to prevent.
	OffBaseline int64 `json:"off_baseline"`
	// Failed counts the requests the host reported as failed on this row. In the channel
	// dimension it is normally zero: a request that failed has no routing block, so it is not
	// in a channel row at all — it is counted in unresolved_requests.
	Failed       int64   `json:"failed"`
	TTFTP50Ms    float64 `json:"ttft_p50_ms"`
	TTFTP90Ms    float64 `json:"ttft_p90_ms"`
	DecodeP50    float64 `json:"decode_p50_tps"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	// CostUSD is the sum of the records' costs: the gateway's own figure for a joined record
	// (gateway_cost), plus the legacy cost block of a v1 line.
	CostUSD float64 `json:"cost_usd"`
}

// ModelStat is one requested model inside the window.
type ModelStat struct {
	Model         string `json:"model"`
	CanonicalSlug string `json:"canonical_slug,omitempty"`
	Requests      int64  `json:"requests"`
	// OffBaseline counts the model's requests whose real channel differs from the baseline.
	// A request with no known channel is not counted here; it is in Hours[].unresolved.
	OffBaseline int64   `json:"off_baseline"`
	TTFTP50Ms   float64 `json:"ttft_p50_ms"`
	DecodeP50   float64 `json:"decode_p50_tps"`
}

// HourPoint is one hour of the timeline. Empty hours are present with zeroes so the chart
// does not have to invent them.
type HourPoint struct {
	Hour        time.Time `json:"hour"`
	Requests    int64     `json:"requests"`
	OffBaseline int64     `json:"off_baseline"`
	Failed      int64     `json:"failed"`
	Fallbacks   int64     `json:"fallbacks"`
	// Unresolved counts the hour's requests whose real channel is unknown: the same split as
	// the window-level unresolved_requests, so a spike that is really a logging gap does not
	// read as a traffic spike.
	Unresolved int64   `json:"unresolved"`
	TTFTP50Ms  float64 `json:"ttft_p50_ms"`
	DecodeP50  float64 `json:"decode_p50_tps"`
	CostUSD    float64 `json:"cost_usd"`
}

// Summary is the answer to the operator question: how many of these requests did not land
// on the expected channel, and how did those requests perform.
//
// It carries two provider dimensions of the same shape, because two different questions are
// asked of one window:
//
//   - providers is the REAL channel: the Cline gateway channel the request landed on,
//     joined in from CPA's request log (finalProvider). off_baseline_requests is measured on
//     this dimension, against baseline_provider, and it means what the page always claimed:
//     the request did not reach the official channel.
//   - cpa_providers is the CPA-side credential the host reported
//     ("openai-compatible-cline1"). It is the dimension this view had before the channel log
//     existed: it says which key carried the request, never which channel served it.
//
// resolved_requests and unresolved_requests split the window by whether a real channel was
// found at all, and that split is the honest denominator: a failed request has no routing
// block, a client that sends no Session_id header can never be matched, and a fact may not
// have been parsed yet. Those records can be counted but not attributed, so they are counted
// as unresolved and are never recorded as baseline hits.
type Summary struct {
	GeneratedAt time.Time `json:"generated_at"`
	Window      string    `json:"window"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Baseline    string    `json:"baseline_provider"`

	// Resolved counts the records that carry a real channel; Unresolved the rest. Only the
	// resolved ones can be attributed to a channel.
	Resolved   int64 `json:"resolved_requests"`
	Unresolved int64 `json:"unresolved_requests"`
	// OffBaseline counts the resolved records whose channel differs from Baseline, so
	// OffRatio = OffBaseline / Resolved.
	OffBaseline int64   `json:"off_baseline_requests"`
	OffRatio    float64 `json:"off_baseline_ratio"`
	// FailedRequests is window-scoped: it counts every record the host reported as failed,
	// whether or not it has a channel. A failed request usually has none, so a channel row's
	// failed count is normally zero and the failure rate is
	// FailedRequests / (Resolved + Unresolved).
	FailedRequests int64 `json:"failed_requests"`
	// Fallbacks counts the resolved records whose gateway needed more than one provider
	// attempt. It is zero before the channel log is switched on.
	Fallbacks int64 `json:"fallback_requests"`
	// Channels counts the real channels the window landed on (len(Providers)).
	Channels int `json:"channels"`

	TTFTP50Ms    float64 `json:"ttft_p50_ms"`
	TTFTP90Ms    float64 `json:"ttft_p90_ms"`
	DecodeP50TPS float64 `json:"decode_p50_tps"`

	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	CostUSD      float64 `json:"cost_usd"`

	Providers    []ProviderStat `json:"providers"`
	CPAProviders []ProviderStat `json:"cpa_providers"`
	Models       []ModelStat    `json:"models"`
	Hours        []HourPoint    `json:"hours"`

	Health Health `json:"health"`
}

// Summary aggregates the window's records.
//
// The records are read from the store on every call, and the facts are read on every call with
// them: the gateway channel of the newest requests arrives seconds after the record does, and
// a summary that cached either half would report a stale answer for as long as the process
// runs. The window is at most seven days of one line per request, which is cheap to read.
func (r *Recorder) Summary(window Window) Summary {
	if r == nil {
		return Summary{Window: string(window)}
	}
	now := r.now()
	from := now.Add(-window.Duration()).Truncate(time.Hour)
	summary := Summary{
		GeneratedAt: now,
		Window:      string(window),
		From:        from,
		To:          now,
		Baseline:    r.options.Baseline,
		Health:      r.Health(),
	}

	records, errLoad := r.RecordsSince(window)
	if errLoad != nil && summary.Health.LastError == "" {
		summary.Health.LastError = errLoad.Error()
	}

	join := r.openJoin()
	channels := newProviderAccumulator()
	credentials := newProviderAccumulator()
	models := map[string]*ModelStat{}
	modelTTFT := map[string][]int64{}
	modelTPS := map[string][]float64{}
	hours := map[int64]*hourAccumulator{}
	var ttfts []int64
	var speeds []float64

	for _, stored := range records {
		record := stored
		fact, matched := join.match(stored)
		if matched {
			// The copy is what the view reads; the stored record is never given a channel.
			record = stored.withChannel(fact)
			summary.Resolved++
		} else {
			summary.Unresolved++
		}
		channel := record.GatewayChannel()
		off := channel != "" && summary.Baseline != "" && !strings.EqualFold(channel, summary.Baseline)
		if off {
			summary.OffBaseline++
		}
		if record.Failed {
			summary.FailedRequests++
		}
		fallback := matched && record.GatewayAttempts > 1
		if fallback {
			summary.Fallbacks++
		}
		cost := record.CostUSD + record.GatewayCost
		summary.InputTokens += record.InputTokens
		summary.OutputTokens += record.OutputTokens
		summary.CachedTokens += record.CachedTokens
		summary.CostUSD += cost
		ttfts = appendSample(ttfts, record.TTFTMs)
		speeds = appendSample(speeds, record.TPS)

		// The credential row is filled for every record that named one, joined or not: the
		// credential is a fact of the usage hook, and the dimension is the one that keeps
		// working while the channel log is switched off.
		credentials.add(record.CPProvider, record, false, cost)
		// The channel row exists only for a joined record, which is why the channel table has
		// no row for a failure.
		channels.add(record.GatewayProvider, record, off, cost)

		model := record.Model
		if model == "" {
			model = record.UpstreamModel
		}
		if model != "" {
			row := models[model]
			if row == nil {
				row = &ModelStat{Model: model, CanonicalSlug: record.CanonicalSlug}
				models[model] = row
			}
			row.Requests++
			if off {
				row.OffBaseline++
			}
			modelTTFT[model] = appendSample(modelTTFT[model], record.TTFTMs)
			modelTPS[model] = appendSample(modelTPS[model], record.TPS)
		}

		hour := record.Time.UTC().Truncate(time.Hour)
		slot := hours[hour.Unix()]
		if slot == nil {
			// The key is the absolute hour; the label is rendered in the location of the
			// window bounds, like the empty points below. Labelled in UTC instead, an hour
			// that holds data would print a different hour from the empty point of the same
			// instant and the timeline would list that hour twice.
			slot = &hourAccumulator{}
			slot.point.Hour = hour.In(from.Location())
			hours[hour.Unix()] = slot
		}
		slot.point.Requests++
		if !matched {
			slot.point.Unresolved++
		}
		if record.Failed {
			slot.point.Failed++
		}
		if fallback {
			slot.point.Fallbacks++
		}
		if off {
			slot.point.OffBaseline++
		}
		slot.point.CostUSD += cost
		slot.ttft = appendSample(slot.ttft, record.TTFTMs)
		slot.tps = appendSample(slot.tps, record.TPS)
	}

	summary.TTFTP50Ms = median(ttfts, 0.5)
	summary.TTFTP90Ms = median(ttfts, 0.9)
	summary.DecodeP50TPS = median(speeds, 0.5)
	if summary.Resolved > 0 {
		summary.OffRatio = float64(summary.OffBaseline) / float64(summary.Resolved)
	}
	summary.Providers = channels.list()
	summary.CPAProviders = credentials.list()
	summary.Channels = len(summary.Providers)

	for _, row := range models {
		row.TTFTP50Ms = median(modelTTFT[row.Model], 0.5)
		row.DecodeP50 = median(modelTPS[row.Model], 0.5)
		summary.Models = append(summary.Models, *row)
	}
	sort.Slice(summary.Models, func(first, second int) bool {
		if summary.Models[first].Requests == summary.Models[second].Requests {
			return summary.Models[first].Model < summary.Models[second].Model
		}
		return summary.Models[first].Requests > summary.Models[second].Requests
	})

	// Every hour of the window gets a point, so a quiet hour shows as a gap rather than
	// moving the neighbouring bars.
	for cursor := from; !cursor.After(now); cursor = cursor.Add(time.Hour) {
		slot := hours[cursor.Unix()]
		if slot == nil {
			summary.Hours = append(summary.Hours, HourPoint{Hour: cursor})
			continue
		}
		point := slot.point
		point.TTFTP50Ms = median(slot.ttft, 0.5)
		point.DecodeP50 = median(slot.tps, 0.5)
		summary.Hours = append(summary.Hours, point)
	}
	return summary
}

// providerAccumulator collects one provider dimension: the rows, their latency samples, and
// the denominator their ratios are computed over. Both dimensions use it, which is what keeps
// the two tables the same shape.
type providerAccumulator struct {
	rows  map[string]*ProviderStat
	ttft  map[string][]int64
	tps   map[string][]float64
	total int64
}

func newProviderAccumulator() *providerAccumulator {
	return &providerAccumulator{
		rows: map[string]*ProviderStat{},
		ttft: map[string][]int64{},
		tps:  map[string][]float64{},
	}
}

// add folds one record into the row of name. An empty name is not a row: a record whose
// credential the host did not report, and a record with no known channel, each stay out of
// their dimension instead of being collected under an empty name.
func (a *providerAccumulator) add(name string, record Record, off bool, cost float64) {
	if a == nil || name == "" {
		return
	}
	row := a.rows[name]
	if row == nil {
		row = &ProviderStat{Provider: name}
		a.rows[name] = row
	}
	a.total++
	row.Requests++
	if off {
		row.OffBaseline++
	}
	if record.Failed {
		row.Failed++
	}
	row.InputTokens += record.InputTokens
	row.OutputTokens += record.OutputTokens
	row.CachedTokens += record.CachedTokens
	row.CostUSD += cost
	a.ttft[name] = appendSample(a.ttft[name], record.TTFTMs)
	a.tps[name] = appendSample(a.tps[name], record.TPS)
}

// list returns the rows, most requests first, with the ratio and the percentiles filled in.
func (a *providerAccumulator) list() []ProviderStat {
	if a == nil {
		return nil
	}
	out := make([]ProviderStat, 0, len(a.rows))
	for name, row := range a.rows {
		entry := *row
		if a.total > 0 {
			entry.Ratio = float64(entry.Requests) / float64(a.total)
		}
		entry.TTFTP50Ms = median(a.ttft[name], 0.5)
		entry.TTFTP90Ms = median(a.ttft[name], 0.9)
		entry.DecodeP50 = median(a.tps[name], 0.5)
		out = append(out, entry)
	}
	sort.Slice(out, func(first, second int) bool {
		if out[first].Requests == out[second].Requests {
			return out[first].Provider < out[second].Provider
		}
		return out[first].Requests > out[second].Requests
	})
	return out
}

// hourAccumulator collects one hour of the timeline: the counts, the cost, and the samples its
// two percentiles are computed from.
type hourAccumulator struct {
	point HourPoint
	ttft  []int64
	tps   []float64
}

// appendSample keeps the newest sampleCap values. A last-N window is an honest description
// of "the recent distribution" and bounds the memory of a long-running process.
func appendSample[T int64 | float64](samples []T, value T) []T {
	if value == 0 {
		return samples
	}
	if len(samples) >= sampleCap {
		copy(samples, samples[1:])
		samples[len(samples)-1] = value
		return samples
	}
	return append(samples, value)
}

// median returns the quantile of a sample set. An empty set is reported as zero, which the
// page renders as "—" rather than as a measurement.
func median[T int64 | float64](samples []T, quantile float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]float64, 0, len(samples))
	for _, sample := range samples {
		sorted = append(sorted, float64(sample))
	}
	sort.Float64s(sorted)
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := quantile * float64(len(sorted)-1)
	lower := int(position)
	upper := lower + 1
	if upper >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	fraction := position - float64(lower)
	return sorted[lower] + (sorted[upper]-sorted[lower])*fraction
}
