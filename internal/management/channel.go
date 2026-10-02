package management

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/wkeking/clinepass-channel-monitor/internal/channellog"
	"github.com/wkeking/clinepass-channel-monitor/internal/observation"
	"github.com/wkeking/clinepass-channel-monitor/internal/plan"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// channelRecordLimit is how many raw records the view carries. They are the rows a number on
// the page can be checked against without going to the file system, which is what the
// acceptance check asks for.
const channelRecordLimit = 20

// channelLogFactLimit is how many parsed request-log facts the payload carries. A fact is a
// candidate for the record it belongs to, so the page only ever needs the newest few, and the
// full history stays in the JSONL store.
const channelLogFactLimit = 20

// channelView is the payload of the 「渠道」 view: the answer for the selected window, the
// newest raw records behind that answer, and the collector's health.
type channelView struct {
	// Enabled distinguishes "observation is switched off" from "nothing happened yet":
	// both produce an empty window, and only the first is a configuration matter.
	Enabled bool                `json:"enabled"`
	Summary observation.Summary `json:"summary"`
	// Records is the newest first, capped at channelRecordLimit; RecordsTotal is how many
	// records the window holds, so the page can say what the cap hides. Each row carries both
	// halves of the request: cpa_provider is the CPA credential the host reported, and the
	// gateway channel (gateway_provider / final_provider, channel_source) is joined in from
	// the CPA request log. A row with an empty channel_source has no known channel — it is not
	// on the baseline, it is unknown.
	Records      []observation.Record `json:"records"`
	RecordsTotal int                  `json:"records_total"`
	Health       observation.Health   `json:"health"`
	// OfficialChannels is the official-source dimension beside the CPA-credential channel
	// above: which upstream inference channel Cline reported for each record, crossed with
	// the model that actually ran. It is always a list, never null, and it is keyed by the
	// collector's retention window rather than the requested one (the caption beside the
	// table states the coverage).
	//
	// It counts successful, billable requests only: a failed request never reaches Cline's
	// usage endpoint, so a failure can only be attributed to a CPA credential and never to
	// an upstream channel.
	OfficialChannels []plan.OfficialChannelRow `json:"official_channels"`
	// OfficialUsage is the collector's own state (enabled, retained items, oldest retained
	// timestamp, last fetch, last error). The page uses it to say how far back the official
	// records reach instead of presenting a partial window as a total.
	OfficialUsage plan.UsageState `json:"official_usage"`
	// ChannelLog is the CPA request-log scanner: whether it runs, what it has read, and the
	// newest facts it parsed. Those facts are the gateway channel the usage records cannot see,
	// and each one carries the session header and arrival timestamp the join is made on.
	ChannelLog channelLogView `json:"channel_log"`
}

// channelLogView is the CPA request-log scanner as a payload. Every field is present on every
// path — including the disabled one — and the fact list is a list, never null, so the page can
// render the state without branching on the shape.
type channelLogView struct {
	Enabled bool `json:"enabled"`
	// DeleteAfterRead and MinAgeSeconds are echoed because they explain what an operator sees:
	// why a file is still on disk, and why one has not been parsed yet.
	DeleteAfterRead bool              `json:"delete_after_read"`
	MinAgeSeconds   int               `json:"min_age_seconds"`
	Health          channellog.Health `json:"health"`
	// Facts is the newest first, capped at channelLogFactLimit; FactsTotal is how many the
	// scanner holds, so the page can say what the cap hides.
	Facts      []channellog.Fact `json:"facts"`
	FactsTotal int               `json:"facts_total"`
}

// channelLogSnapshot reads the scanner out of the published state. A scanner that is switched
// off produces the same shape as one that is on with nothing to report: only the values differ.
func channelLogSnapshot() channelLogView {
	cfg := state.Config()
	view := channelLogView{
		Enabled:         cfg.ChannelLogEnabled,
		DeleteAfterRead: cfg.ChannelLogDeleteAfterRead,
		MinAgeSeconds:   cfg.ChannelLogMinAgeSeconds,
		Health:          channellog.Health{Enabled: cfg.ChannelLogEnabled, Directory: cfg.ChannelLogDir},
		Facts:           []channellog.Fact{},
	}
	scanner := state.ChannelLog()
	if scanner == nil {
		if cfg.ChannelLogEnabled {
			// Switched on with nothing reading: an operator looking at an empty page needs to
			// be told it is a wiring state, not a quiet directory.
			view.Health.LastError = "scanner is not running"
		}
		return view
	}
	// The running scanner's own options are what actually govern it, which is why they are
	// reported in preference to the configuration block.
	options := scanner.Options()
	health := scanner.Health()
	health.Enabled = true
	view.Enabled = true
	view.DeleteAfterRead = options.DeleteAfterRead
	view.MinAgeSeconds = int(options.MinAge / time.Second)
	view.Health = health
	facts := scanner.Facts()
	view.FactsTotal = len(facts)
	if len(facts) > channelLogFactLimit {
		facts = facts[:channelLogFactLimit]
	}
	if facts == nil {
		facts = []channellog.Fact{}
	}
	view.Facts = facts
	return view
}

// officialChannelView reads the official-usage dimension out of the plan poller. Both halves
// stay empty until a collector has fetched something: a deployment with the plan poller
// switched off, or one that has not reached Cline yet, still gets the key as an empty list
// plus the metadata that explains it, never a null the page would have to special-case.
func officialChannelView() ([]plan.OfficialChannelRow, plan.UsageState) {
	rows := []plan.OfficialChannelRow{}
	poller := state.Plan()
	if poller == nil {
		return rows, plan.UsageState{}
	}
	snapshot := poller.Snapshot()
	if len(snapshot.OfficialChannels) > 0 {
		rows = snapshot.OfficialChannels
	}
	return rows, snapshot.Usage
}

// buildChannelView answers one window of channel observation.
func buildChannelView(req *pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	window, ok := observation.ParseWindow(req.Query.Get("window"))
	if !ok {
		return errorResponse(http.StatusBadRequest, "invalid_window", "window must be 1h, 24h or 7d")
	}
	officialChannels, officialUsage := officialChannelView()
	channelLog := channelLogSnapshot()
	recorder := state.Observation()
	if recorder == nil {
		cfg := state.Config()
		return jsonResponse(channelView{
			Enabled:          false,
			Summary:          observation.Summary{Window: string(window)},
			Health:           observation.Health{Directory: cfg.ChannelStoreDir},
			OfficialChannels: officialChannels,
			OfficialUsage:    officialUsage,
			ChannelLog:       channelLog,
		})
	}
	summary := recorder.Summary(window)
	records, _ := recorder.ChannelRecordsSince(window)
	total := len(records)
	if total > channelRecordLimit {
		records = records[:channelRecordLimit]
	}
	if records == nil {
		records = []observation.Record{}
	}
	return jsonResponse(channelView{
		Enabled:          true,
		Summary:          summary,
		Records:          records,
		RecordsTotal:     total,
		Health:           summary.Health,
		OfficialChannels: officialChannels,
		OfficialUsage:    officialUsage,
		ChannelLog:       channelLog,
	})
}

// buildChannelCSV streams the same window as CSV. It is the export the acceptance check
// names: one row per request, with the channel fields included.
func buildChannelCSV(req *pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	window, ok := observation.ParseWindow(req.Query.Get("window"))
	if !ok {
		return errorResponse(http.StatusBadRequest, "invalid_window", "window must be 1h, 24h or 7d")
	}
	buffer := &bytes.Buffer{}
	rows := 0
	if recorder := state.Observation(); recorder != nil {
		written, errWrite := recorder.WriteCSV(window, buffer)
		if errWrite != nil {
			// A partial export beats no export, but the caller has to know: the header
			// carries how many rows made it and the file is still sent.
			buffer.WriteString(fmt.Sprintf("# export stopped after %d rows: %s\n", written, errWrite.Error()))
		}
		rows = written
	}
	filename := fmt.Sprintf("channel-observation-%s-%s.csv", window, time.Now().UTC().Format("20060102T150405Z"))
	headers := csvHeaders(filename)
	headers.Set("X-Record-Count", strconv.Itoa(rows))
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: headers, Body: buffer.Bytes()}
}

func csvHeaders(filename string) http.Header {
	return http.Header{
		"Content-Type":           []string{"text/csv; charset=utf-8"},
		"Content-Disposition":    []string{`attachment; filename="` + filename + `"`},
		"Cache-Control":          []string{"no-store"},
		"X-Content-Type-Options": []string{"nosniff"},
	}
}

// observationHealth is what /health reports about the collector, including the disabled
// case so a deployment can see the switch and the directory without reading the config.
func observationHealth() observation.Health {
	cfg := state.Config()
	recorder := state.Observation()
	if recorder == nil {
		health := observation.Health{Enabled: false, Directory: cfg.ChannelStoreDir}
		if cfg.ChannelObserveEnabled {
			health.Enabled = true
			health.LastError = "collector is not running"
		}
		return health
	}
	health := recorder.Health()
	health.Enabled = true
	return health
}

func errorResponse(status int, code, message string) pluginapi.ManagementResponse {
	body := []byte(`{"error":"` + code + `","message":"` + message + `"}`)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: jsonHeaders(), Body: body}
}
