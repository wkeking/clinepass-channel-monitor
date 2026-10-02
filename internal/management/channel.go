package management

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/wkeking/clinepass-channel-monitor/internal/observation"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// channelRecordLimit is how many raw records the view carries. They are the rows a number on
// the page can be checked against without going to the file system, which is what the
// acceptance check asks for.
const channelRecordLimit = 20

// channelView is the payload of the 「渠道」 view: the answer for the selected window, the
// newest raw records behind that answer, and the collector's health.
type channelView struct {
	// Enabled distinguishes "observation is switched off" from "nothing happened yet":
	// both produce an empty window, and only the first is a configuration matter.
	Enabled bool                `json:"enabled"`
	Summary observation.Summary `json:"summary"`
	// Records is the newest first, capped at channelRecordLimit; RecordsTotal is how many
	// records the window holds, so the page can say what the cap hides.
	Records      []observation.Record `json:"records"`
	RecordsTotal int                  `json:"records_total"`
	Health       observation.Health   `json:"health"`
}

// buildChannelView answers one window of channel observation.
func buildChannelView(req *pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	window, ok := observation.ParseWindow(req.Query.Get("window"))
	if !ok {
		return errorResponse(http.StatusBadRequest, "invalid_window", "window must be 1h, 24h or 7d")
	}
	recorder := state.Observation()
	if recorder == nil {
		cfg := state.Config()
		return jsonResponse(channelView{
			Enabled: false,
			Summary: observation.Summary{Window: string(window)},
			Health:  observation.Health{Directory: cfg.ChannelStoreDir},
		})
	}
	summary := recorder.Summary(window)
	records, _ := recorder.RecordsSince(window)
	total := len(records)
	if total > channelRecordLimit {
		records = records[:channelRecordLimit]
	}
	if records == nil {
		records = []observation.Record{}
	}
	return jsonResponse(channelView{
		Enabled:      true,
		Summary:      summary,
		Records:      records,
		RecordsTotal: total,
		Health:       summary.Health,
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
