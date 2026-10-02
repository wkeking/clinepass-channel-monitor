package observation

import (
	"sort"
	"strings"
	"sync"
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

// ProviderStat is one upstream channel inside the window.
type ProviderStat struct {
	Provider    string  `json:"provider"`
	Requests    int64   `json:"requests"`
	Ratio       float64 `json:"ratio"`
	OffBaseline int64   `json:"off_baseline"`
	// Failed counts the requests the host reported as failed on this channel: the number
	// that separates "this channel is slow" from "this channel is broken".
	Failed       int64   `json:"failed"`
	TTFTP50Ms    float64 `json:"ttft_p50_ms"`
	TTFTP90Ms    float64 `json:"ttft_p90_ms"`
	DecodeP50    float64 `json:"decode_p50_tps"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

// ModelStat is one requested model inside the window.
type ModelStat struct {
	Model         string  `json:"model"`
	CanonicalSlug string  `json:"canonical_slug,omitempty"`
	Requests      int64   `json:"requests"`
	OffBaseline   int64   `json:"off_baseline"`
	TTFTP50Ms     float64 `json:"ttft_p50_ms"`
	DecodeP50     float64 `json:"decode_p50_tps"`
}

// HourPoint is one hour of the timeline. Empty hours are present with zeroes so the chart
// does not have to invent them.
type HourPoint struct {
	Hour        time.Time `json:"hour"`
	Requests    int64     `json:"requests"`
	OffBaseline int64     `json:"off_baseline"`
	Failed      int64     `json:"failed"`
	Fallbacks   int64     `json:"fallbacks"`
	TTFTP50Ms   float64   `json:"ttft_p50_ms"`
	DecodeP50   float64   `json:"decode_p50_tps"`
	CostUSD     float64   `json:"cost_usd"`
}

// Summary is the answer to the operator question: how many of these requests did not land
// on the expected channel, and how did those requests perform.
type Summary struct {
	GeneratedAt time.Time `json:"generated_at"`
	Window      string    `json:"window"`
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	Baseline    string    `json:"baseline_provider"`

	// Resolved counts the requests whose upstream reported a channel; those are the only
	// ones a ratio can be computed from. Unresolved is reported by Health.
	Resolved    int64   `json:"resolved_requests"`
	OffBaseline int64   `json:"off_baseline_requests"`
	OffRatio    float64 `json:"off_baseline_ratio"`
	// FailedRequests counts the records the host reported as failed; a window's failure rate
	// is FailedRequests / Resolved.
	FailedRequests int64 `json:"failed_requests"`
	Fallbacks      int64 `json:"fallback_requests"`
	Channels       int   `json:"channels"`

	TTFTP50Ms    float64 `json:"ttft_p50_ms"`
	TTFTP90Ms    float64 `json:"ttft_p90_ms"`
	DecodeP50TPS float64 `json:"decode_p50_tps"`

	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	CostUSD      float64 `json:"cost_usd"`

	Providers []ProviderStat `json:"providers"`
	Models    []ModelStat    `json:"models"`
	Hours     []HourPoint    `json:"hours"`

	Health Health `json:"health"`
}

// Summary aggregates the buckets inside the window.
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

	r.mu.Lock()
	selected := r.agg.between(from, now)
	r.mu.Unlock()

	providers := map[string]*ProviderStat{}
	models := map[string]*ModelStat{}
	hours := map[int64]*HourPoint{}
	providerTTFT := map[string][]int64{}
	providerTPS := map[string][]float64{}
	modelTTFT := map[string][]int64{}
	modelTPS := map[string][]float64{}
	var ttfts []int64
	var speeds []float64

	for _, bucket := range selected {
		hour := bucket.hour
		point := hours[hour.Unix()]
		if point == nil {
			// The bucket key is an absolute hour; the label is rendered in the location of the
			// window bounds, like the empty points below. Labelled in UTC instead, a bucket that
			// holds data would print a different hour from the empty point of the same instant
			// and the timeline would list that hour twice.
			point = &HourPoint{Hour: hour.In(from.Location())}
			hours[hour.Unix()] = point
		}
		point.Requests += bucket.requests
		point.OffBaseline += bucket.off
		point.Failed += bucket.failed
		point.Fallbacks += bucket.fallback
		point.CostUSD += bucket.cost
		point.TTFTP50Ms = median(bucket.ttft, 0.5)
		point.DecodeP50 = median(bucket.tps, 0.5)

		summary.Resolved += bucket.requests
		summary.OffBaseline += bucket.off
		summary.FailedRequests += bucket.failed
		summary.Fallbacks += bucket.fallback
		summary.InputTokens += bucket.input
		summary.OutputTokens += bucket.output
		summary.CachedTokens += bucket.cached
		summary.CostUSD += bucket.cost
		ttfts = mergeSamples(ttfts, bucket.ttft)
		speeds = mergeSamples(speeds, bucket.tps)

		for name, entry := range bucket.providers {
			target := providers[name]
			if target == nil {
				target = &ProviderStat{Provider: name}
				providers[name] = target
			}
			target.Requests += entry.requests
			target.OffBaseline += entry.off
			target.Failed += entry.failed
			target.InputTokens += entry.input
			target.OutputTokens += entry.output
			target.CachedTokens += entry.cached
			target.CostUSD += entry.cost
			providerTTFT[name] = mergeSamples(providerTTFT[name], entry.ttft)
			providerTPS[name] = mergeSamples(providerTPS[name], entry.tps)
		}
		for name, entry := range bucket.models {
			target := models[name]
			if target == nil {
				target = &ModelStat{Model: name, CanonicalSlug: entry.slug}
				models[name] = target
			}
			target.Requests += entry.requests
			target.OffBaseline += entry.off
			modelTTFT[name] = mergeSamples(modelTTFT[name], entry.ttft)
			modelTPS[name] = mergeSamples(modelTPS[name], entry.tps)
		}
	}

	summary.TTFTP50Ms = median(ttfts, 0.5)
	summary.TTFTP90Ms = median(ttfts, 0.9)
	summary.DecodeP50TPS = median(speeds, 0.5)
	if summary.Resolved > 0 {
		summary.OffRatio = float64(summary.OffBaseline) / float64(summary.Resolved)
	}

	for name, entry := range providers {
		if summary.Resolved > 0 {
			entry.Ratio = float64(entry.Requests) / float64(summary.Resolved)
		}
		entry.TTFTP50Ms = median(providerTTFT[name], 0.5)
		entry.TTFTP90Ms = median(providerTTFT[name], 0.9)
		entry.DecodeP50 = median(providerTPS[name], 0.5)
		summary.Providers = append(summary.Providers, *entry)
	}
	sort.Slice(summary.Providers, func(first, second int) bool {
		if summary.Providers[first].Requests == summary.Providers[second].Requests {
			return summary.Providers[first].Provider < summary.Providers[second].Provider
		}
		return summary.Providers[first].Requests > summary.Providers[second].Requests
	})
	summary.Channels = len(summary.Providers)

	for _, entry := range models {
		entry.TTFTP50Ms = median(modelTTFT[entry.Model], 0.5)
		entry.DecodeP50 = median(modelTPS[entry.Model], 0.5)
		summary.Models = append(summary.Models, *entry)
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
		point := hours[cursor.Unix()]
		if point == nil {
			point = &HourPoint{Hour: cursor}
		}
		summary.Hours = append(summary.Hours, *point)
	}
	return summary
}

// mergeSamples folds one bucket's samples into a running set, honouring the same cap as the
// buckets themselves. The cap bounds the union too, so a seven-day window costs no more
// memory than a one-hour one.
func mergeSamples[T int64 | float64](into, from []T) []T {
	for _, sample := range from {
		into = appendSample(into, sample)
	}
	return into
}

// aggregate is a ring of hourly buckets. A bucket that falls out of the ring is discarded,
// which is what keeps memory bounded for a process that runs for months.
type aggregate struct {
	mu      sync.Mutex
	buckets [bucketSlots]*bucket
}

type bucket struct {
	hour     time.Time
	requests int64
	off      int64
	failed   int64
	fallback int64
	input    int64
	output   int64
	cached   int64
	cost     float64
	ttft     []int64
	tps      []float64

	providers map[string]*providerAgg
	models    map[string]*modelAgg
}

type providerAgg struct {
	requests int64
	off      int64
	failed   int64
	input    int64
	output   int64
	cached   int64
	cost     float64
	ttft     []int64
	tps      []float64
}

type modelAgg struct {
	requests int64
	off      int64
	slug     string
	ttft     []int64
	tps      []float64
}

func newAggregate() *aggregate {
	return &aggregate{}
}

func newBucket(hour time.Time) *bucket {
	return &bucket{
		hour:      hour,
		providers: map[string]*providerAgg{},
		models:    map[string]*modelAgg{},
	}
}

// add folds one record into its hour. The aggregate lock is separate from the recorder
// lock so a query never waits behind the writer.
func (a *aggregate) add(record Record, baseline string) {
	if a == nil {
		return
	}
	hour := record.Time.UTC().Truncate(time.Hour)
	slot := int(hour.Unix()/3600) % bucketSlots
	if slot < 0 {
		slot += bucketSlots
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	current := a.buckets[slot]
	if current == nil || !current.hour.Equal(hour) {
		current = newBucket(hour)
		a.buckets[slot] = current
	}

	provider := record.FinalProvider
	if provider == "" {
		// A v1 line can carry only the resolved side; a v2 record sets both from the
		// credential the host reported.
		provider = record.ResolvedProvider
	}
	off := int64(0)
	if provider != "" && baseline != "" && !strings.EqualFold(provider, baseline) {
		off = 1
	}

	failed := int64(0)
	if record.Failed {
		failed = 1
	}

	current.requests++
	current.off += off
	current.failed += failed
	// The fallback counters came from the gateway routing block, which the usage payload does
	// not carry: they stay zero instead of being guessed at.
	if record.Attempts > 1 || record.ModelAttempts > 1 {
		current.fallback++
	}
	current.input += record.InputTokens
	current.output += record.OutputTokens
	current.cached += record.CachedTokens
	current.cost += record.CostUSD
	current.ttft = appendSample(current.ttft, record.TTFTMs)
	current.tps = appendSample(current.tps, record.TPS)

	if provider != "" {
		entry := current.providers[provider]
		if entry == nil {
			entry = &providerAgg{}
			current.providers[provider] = entry
		}
		entry.requests++
		entry.off += off
		entry.failed += failed
		entry.input += record.InputTokens
		entry.output += record.OutputTokens
		entry.cached += record.CachedTokens
		entry.cost += record.CostUSD
		entry.ttft = appendSample(entry.ttft, record.TTFTMs)
		entry.tps = appendSample(entry.tps, record.TPS)
	}

	model := record.Model
	if model == "" {
		model = record.UpstreamModel
	}
	if model != "" {
		entry := current.models[model]
		if entry == nil {
			entry = &modelAgg{slug: record.CanonicalSlug}
			current.models[model] = entry
		}
		entry.requests++
		entry.off += off
		entry.ttft = appendSample(entry.ttft, record.TTFTMs)
		entry.tps = appendSample(entry.tps, record.TPS)
	}
}

// between returns the buckets inside the range, oldest first.
func (a *aggregate) between(from, to time.Time) []*bucket {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*bucket, 0, 32)
	for _, bucket := range a.buckets {
		if bucket == nil {
			continue
		}
		if bucket.hour.Before(from.Truncate(time.Hour)) || bucket.hour.After(to) {
			continue
		}
		out = append(out, bucket)
	}
	sort.Slice(out, func(first, second int) bool {
		return out[first].hour.Before(out[second].hour)
	})
	return out
}

// appendSample keeps the newest sampleCap values. A last-N window is a honest description
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
