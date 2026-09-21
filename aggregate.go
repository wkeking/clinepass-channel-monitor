package main

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// parseCost turns a gateway cost string into a number. The gateway sends plain
// decimal strings such as "0.0000447"; anything unparseable is counted instead of
// being silently dropped.
func parseCost(value string) (float64, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	parsed, errParse := strconv.ParseFloat(trimmed, 64)
	if errParse != nil {
		return 0, false
	}
	return parsed, true
}

// promptCacheAccumulator keeps the ratio of cache hits to all prompt tokens.
type promptCacheAccumulator struct {
	Hit  int64 `json:"hit"`
	Miss int64 `json:"miss"`
}

func (a *promptCacheAccumulator) add(hit, miss int64) {
	a.Hit += hit
	a.Miss += miss
}

func (a promptCacheAccumulator) ratio() float64 {
	total := a.Hit + a.Miss
	if total <= 0 {
		return 0
	}
	return float64(a.Hit) / float64(total)
}

// tokenAccumulator keeps mean values for per-request token fields.
type tokenAccumulator struct {
	Requests  int64   `json:"requests"`
	Input     int64   `json:"input"`
	Output    int64   `json:"output"`
	Reasoning int64   `json:"reasoning"`
	Total     int64   `json:"total"`
	Cached    int64   `json:"cached"`
	AvgInput  float64 `json:"avg_input"`
	AvgOutput float64 `json:"avg_output"`
}

func (a *tokenAccumulator) add(e *event) {
	a.Requests++
	a.Input += e.InputTokens
	a.Output += e.OutputTokens
	a.Reasoning += e.ReasoningTokens
	a.Total += e.TotalTokens
	a.Cached += e.CachedTokens
}

func (a *tokenAccumulator) finalize() {
	if a.Requests > 0 {
		a.AvgInput = float64(a.Input) / float64(a.Requests)
		a.AvgOutput = float64(a.Output) / float64(a.Requests)
	}
}

// costAccumulator sums the gateway cost strings. When a value cannot be parsed the
// affected event count is reported so the total is never silently wrong.
type costAccumulator struct {
	Total    float64 `json:"total"`
	Parsed   int64   `json:"parsed"`
	Unparsed int64   `json:"unparsed"`
}

func (a *costAccumulator) add(value string) {
	if value == "" {
		return
	}
	parsed, ok := parseCost(value)
	if !ok {
		a.Unparsed++
		return
	}
	a.Total += parsed
	a.Parsed++
}

// channelStat is one row of the channel / model / source distribution tables.
type channelStat struct {
	Key             string  `json:"key"`
	Requests        int64   `json:"requests"`
	Failed          int64   `json:"failed"`
	Cost            float64 `json:"cost"`
	AvgLatencyMS    float64 `json:"avg_latency_ms"`
	AvgTTFTMS       float64 `json:"avg_ttft_ms"`
	AvgInputTokens  float64 `json:"avg_input_tokens"`
	AvgOutputTokens float64 `json:"avg_output_tokens"`
	ErrorRate       float64 `json:"error_rate"`
	// AvgCachedTokens is the CPA-side cached token average for this dimension.
	AvgCachedTokens float64 `json:"avg_cached_tokens"`
	// PromptCacheHitTokens / PromptCacheMissTokens are the upstream cache counters; the
	// page shows their ratio, which is a different measurement than the CPA-side cache.
	PromptCacheHitTokens  int64   `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int64   `json:"prompt_cache_miss_tokens"`
	PromptCacheRatio      float64 `json:"prompt_cache_ratio"`
}

// windowStats is the aggregation returned by /stats for one time window.
type windowStats struct {
	Window string `json:"window"`
	From   string `json:"from"`
	To     string `json:"to"`

	Requests       int64   `json:"requests"`
	Failed         int64   `json:"failed"`
	ErrorRate      float64 `json:"error_rate"`
	ChannelMissing int64   `json:"channel_missing"`

	Tokens     tokenAccumulator `json:"tokens"`
	Cost       costAccumulator  `json:"cost"`
	AvgLatency float64          `json:"avg_latency_ms"`
	AvgTTFT    float64          `json:"avg_ttft_ms"`
	// TTFTRequests counts the requests that actually reported a TTFT.
	TTFTRequests int64 `json:"ttft_requests"`
	// TPSAvg is the mean generation speed over requests that produced output.
	TPSAvg float64 `json:"tokens_per_second_avg"`
	// CacheRatio is the CPA-side cache read ratio over all input tokens.
	CacheRatio float64 `json:"cache_ratio"`
	// PromptCache is the upstream-side prompt cache ratio reported by the gateway.
	PromptCache promptCacheAccumulator `json:"prompt_cache"`

	Channels []channelStat `json:"channels"`
	Models   []channelStat `json:"models"`
	Sources  []channelStat `json:"sources"`

	// Events is the number of ring-buffer events this summary was computed from.
	Events int `json:"events"`
	// Complete is false when the requested window reaches further back than the ring buffer.
	Complete bool `json:"complete"`
}

// statsWindow replays the in-memory ring for one window. It is the only place that
// walks the ring for aggregation, so the cost stays linear in the buffer size.
func (s *store) statsWindow(window time.Duration, label string, now time.Time) *windowStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := &windowStats{Window: label, Complete: true}
	cutoff := now.Add(-window)
	stats.To = now.Format(time.RFC3339)
	stats.From = cutoff.Format(time.RFC3339)

	channels := map[string]*channelStat{}
	models := map[string]*channelStat{}
	sources := map[string]*channelStat{}

	var latencySum, ttftSum int64
	oldest := now
	for i := 0; i < s.size; i++ {
		index := (s.head - 1 - i + len(s.ring)) % len(s.ring)
		if index < 0 {
			continue
		}
		e := s.ring[index]
		if e == nil {
			continue
		}
		if e.Timestamp.Before(oldest) {
			oldest = e.Timestamp
		}
		if e.Timestamp.Before(cutoff) {
			break
		}
		stats.Events++
		stats.Requests++
		if e.Failed {
			stats.Failed++
		}
		if e.ChannelMissing {
			stats.ChannelMissing++
		}
		stats.Tokens.add(e)
		if s.cfg.CaptureCost {
			stats.Cost.add(e.Cost)
		}
		latencySum += e.LatencyMS
		if e.TTFTMS > 0 {
			ttftSum += e.TTFTMS
			stats.TTFTRequests++
		}
		if e.TokensPerSecond > 0 {
			stats.TPSAvg += e.TokensPerSecond
		}
		if s.cfg.CaptureCache {
			stats.PromptCache.add(e.PromptCacheHitTokens, e.PromptCacheMissTokens)
		}
		accumulate(channels, e.FinalProvider, e)
		accumulate(models, e.Model, e)
		accumulate(sources, e.APIKey, e)
	}
	if oldest.After(cutoff) && s.size >= len(s.ring) && len(s.ring) > 0 {
		// The ring wrapped, so the requested window may be truncated.
		stats.Complete = false
	}
	stats.Tokens.finalize()
	if stats.Requests > 0 {
		stats.ErrorRate = float64(stats.Failed) / float64(stats.Requests)
		stats.AvgLatency = float64(latencySum) / float64(stats.Requests)
		stats.TPSAvg = stats.TPSAvg / float64(stats.Requests)
	}
	if stats.TTFTRequests > 0 {
		stats.AvgTTFT = float64(ttftSum) / float64(stats.TTFTRequests)
	}
	if stats.Tokens.Input > 0 {
		stats.CacheRatio = float64(stats.Tokens.Cached) / float64(stats.Tokens.Input)
	}
	stats.Channels = finalizeStats(channels)
	stats.Models = finalizeStats(models)
	stats.Sources = finalizeStats(sources)
	return stats
}

func accumulate(target map[string]*channelStat, key string, e *event) {
	if key == "" {
		key = "(unknown)"
	}
	stat, ok := target[key]
	if !ok {
		stat = &channelStat{Key: key}
		target[key] = stat
	}
	stat.Requests++
	if e.Failed {
		stat.Failed++
	}
	if parsed, okCost := parseCost(e.Cost); okCost {
		stat.Cost += parsed
	}
	stat.AvgLatencyMS += float64(e.LatencyMS)
	stat.AvgTTFTMS += float64(e.TTFTMS)
	stat.AvgInputTokens += float64(e.InputTokens)
	stat.AvgOutputTokens += float64(e.OutputTokens)
	stat.AvgCachedTokens += float64(e.CachedTokens)
	stat.PromptCacheHitTokens += e.PromptCacheHitTokens
	stat.PromptCacheMissTokens += e.PromptCacheMissTokens
}

// ratioOf returns hit/(hit+miss), or 0 when no counter is available.
func ratioOf(hit, miss int64) float64 {
	total := hit + miss
	if total <= 0 {
		return 0
	}
	return float64(hit) / float64(total)
}

func finalizeStats(source map[string]*channelStat) []channelStat {
	out := make([]channelStat, 0, len(source))
	for _, stat := range source {
		if stat.Requests > 0 {
			stat.AvgLatencyMS /= float64(stat.Requests)
			stat.AvgTTFTMS /= float64(stat.Requests)
			stat.AvgInputTokens /= float64(stat.Requests)
			stat.AvgOutputTokens /= float64(stat.Requests)
			stat.AvgCachedTokens /= float64(stat.Requests)
			stat.ErrorRate = float64(stat.Failed) / float64(stat.Requests)
			stat.PromptCacheRatio = ratioOf(stat.PromptCacheHitTokens, stat.PromptCacheMissTokens)
		}
		out = append(out, *stat)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests == out[j].Requests {
			return out[i].Key < out[j].Key
		}
		return out[i].Requests > out[j].Requests
	})
	return out
}
