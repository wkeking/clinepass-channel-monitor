package observation

import (
	"strconv"
	"strings"
	"time"
)

// csvHeader is the fixed column order of the export. It is written even when the window has
// no records, so a spreadsheet template can be built from an empty export.
//
// The columns are the ones a usage-hook record can fill, plus the v1 columns that still have
// a source on a line written by the retired stream-sniffing build: those rows keep reading
// back, and a column with an empty cell is cheaper than a schema the operator has to know
// about.
func csvHeader() []string {
	return []string{
		"time_utc",
		"request_id",
		"session_id",
		"generation_id",
		"model",
		"alias",
		"upstream_model",
		"canonical_slug",
		"original_model_id",
		"final_provider",
		"resolved_provider",
		"pinned_provider",
		"auth_id",
		"auth_index",
		"auth_type",
		"executor_type",
		"reasoning_effort",
		"service_tier",
		"upstream_request_id",
		"off_baseline",
		"status_code",
		"failed",
		"protocol",
		"ttft_ms",
		"duration_ms",
		"decode_ms",
		"tokens_per_second",
		"input_tokens",
		"output_tokens",
		"reasoning_tokens",
		"cached_tokens",
		"cache_read_tokens",
		"cache_creation_tokens",
		"total_tokens",
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
		record.Alias,
		record.UpstreamModel,
		record.CanonicalSlug,
		record.OriginalModel,
		record.FinalProvider,
		record.ResolvedProvider,
		record.PinnedProvider,
		record.AuthID,
		record.AuthIndex,
		record.AuthType,
		record.ExecutorType,
		record.ReasoningEffort,
		record.ServiceTier,
		record.UpstreamRequestID,
		off,
		strconv.Itoa(record.StatusCode),
		strconv.FormatBool(record.Failed),
		record.Protocol,
		strconv.FormatInt(record.TTFTMs, 10),
		strconv.FormatInt(record.DurationMs, 10),
		strconv.FormatInt(record.DecodeMs, 10),
		strconv.FormatFloat(record.TPS, 'f', 2, 64),
		strconv.FormatInt(record.InputTokens, 10),
		strconv.FormatInt(record.OutputTokens, 10),
		strconv.FormatInt(record.ReasoningTokens, 10),
		strconv.FormatInt(record.CachedTokens, 10),
		strconv.FormatInt(record.CacheReadTokens, 10),
		strconv.FormatInt(record.CacheCreationTokens, 10),
		strconv.FormatInt(record.TotalTokens, 10),
	}
}

// recordChannel is the channel a request actually landed on. Both record fields carry the
// credential the host reported; resolved_provider covers a v1 line that only carries the
// second one.
func recordChannel(record Record) string {
	if strings.TrimSpace(record.FinalProvider) != "" {
		return record.FinalProvider
	}
	return record.ResolvedProvider
}
