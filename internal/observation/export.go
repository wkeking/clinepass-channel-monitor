package observation

import (
	"strconv"
	"strings"
	"time"
)

// csvHeader is the fixed column order of the export. It is written even when the window has
// no records, so a spreadsheet template can be built from an empty export.
func csvHeader() []string {
	return []string{
		"time_utc",
		"request_id",
		"session_id",
		"generation_id",
		"model",
		"upstream_model",
		"canonical_slug",
		"original_model_id",
		"final_provider",
		"resolved_provider",
		"pinned_provider",
		"affinity",
		"upstream_request_id",
		"model_attempts",
		"provider_attempts",
		"fallbacks_available",
		"off_baseline",
		"status_code",
		"protocol",
		"ttft_ms",
		"duration_ms",
		"decode_ms",
		"tokens_per_second",
		"input_tokens",
		"output_tokens",
		"reasoning_tokens",
		"cached_tokens",
		"total_tokens",
		"cost_usd",
		"frames",
		"user_agent",
		"claude_code_version",
	}
}

// csvRecord renders one stored record in the header's column order. The baseline is passed
// in because "off baseline" is a property of the question being asked, not of the record:
// the same row is off-baseline against one provider and on-baseline against another.
func csvRecord(record Record, baseline string) []string {
	off := ""
	if provider := recordChannel(record); provider != "" && baseline != "" && !strings.EqualFold(provider, baseline) {
		off = "yes"
	}
	return []string{
		record.Time.UTC().Format(time.RFC3339),
		record.RequestID,
		record.SessionID,
		record.GenerationID,
		record.Model,
		record.UpstreamModel,
		record.CanonicalSlug,
		record.OriginalModel,
		record.FinalProvider,
		record.ResolvedProvider,
		record.PinnedProvider,
		record.AffinityOutcome,
		record.UpstreamRequestID,
		strconv.Itoa(record.ModelAttempts),
		strconv.Itoa(record.Attempts),
		strconv.Itoa(len(record.Fallbacks)),
		off,
		strconv.Itoa(record.StatusCode),
		record.Protocol,
		strconv.FormatInt(record.TTFTMs, 10),
		strconv.FormatInt(record.DurationMs, 10),
		strconv.FormatInt(record.DecodeMs, 10),
		strconv.FormatFloat(record.TPS, 'f', 2, 64),
		strconv.FormatInt(record.InputTokens, 10),
		strconv.FormatInt(record.OutputTokens, 10),
		strconv.FormatInt(record.ReasoningTokens, 10),
		strconv.FormatInt(record.CachedTokens, 10),
		strconv.FormatInt(record.TotalTokens, 10),
		strconv.FormatFloat(record.CostUSD, 'f', 6, 64),
		strconv.Itoa(record.Frames),
		record.UserAgent,
		record.ClaudeCodeVersion,
	}
}

// recordChannel is the channel a request actually landed on. The upstream reports
// finalProvider on the routing block; resolvedProvider covers a frame that only carries the
// second one.
func recordChannel(record Record) string {
	if strings.TrimSpace(record.FinalProvider) != "" {
		return record.FinalProvider
	}
	return record.ResolvedProvider
}
