package observation

import (
	"strconv"
	"strings"
	"time"
)

// csvHeader is the fixed column order of the export. It is written even when the window has
// no records, so a spreadsheet template can be built from an empty export.
//
// The columns are the ones a written record can fill — plus the gateway channel, which the
// reader joins in before the row is rendered, and the v1 columns that still have a source on a
// line written by the retired stream-sniffing build. A column with an empty cell is cheaper
// than a schema the operator has to know about.
//
// Two things about the channel columns are worth stating in one place, because a spreadsheet
// row hides them:
//
//   - cpa_provider is the CPA-side credential. Rows written before schema v3 carried the same
//     value under final_provider / resolved_provider and are read back here, so an old window
//     exports with its credential intact and its gateway columns empty.
//   - the gateway_* columns and channel_source are empty on a row whose request log named no
//     channel: a failure, a client that sent no Session_id header, a fact that had not been
//     parsed yet. Empty means "not known"; it never means "on the baseline".
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
		"cpa_provider",
		"gateway_provider",
		"gateway_resolved_provider",
		"gateway_slug",
		"gateway_attempts",
		"gateway_cost",
		"channel_source",
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

// csvRecord renders one view record in the header's column order. The baseline is passed in
// because "off baseline" is a property of the question being asked, not of the record: the
// same row is off-baseline against one channel and on-baseline against another.
//
// off_baseline marks the real channel only. A row with no known channel leaves the cell empty,
// which is what keeps a 502 from being exported as a baseline hit.
func csvRecord(record Record, baseline string) []string {
	off := ""
	if channel := record.GatewayChannel(); channel != "" && baseline != "" && !strings.EqualFold(channel, baseline) {
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
		record.CPProvider,
		record.GatewayProvider,
		record.GatewayResolvedProvider,
		record.GatewaySlug,
		strconv.Itoa(record.GatewayAttempts),
		strconv.FormatFloat(record.GatewayCost, 'f', 8, 64),
		record.ChannelSource,
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
