// Package state holds the process-wide runtime state: the active configuration, the
// Cline usage poller, the channel-observation collector and the CPA request-log scanner.
//
// The plugin package publishes it on load and on every reconfigure; the management API
// reads it. Keeping it in its own package is what lets those packages depend on one
// another without cycles.
package state

import (
	"sync"

	"github.com/wkeking/clinepass-channel-monitor/internal/channellog"
	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/observation"
	"github.com/wkeking/clinepass-channel-monitor/internal/plan"
)

var (
	mu   sync.RWMutex
	cfg  = config.Default()
	pol  *plan.Poller
	obs  *observation.Recorder
	clog *channellog.Scanner
)

// Config returns the active configuration (defaults before the first load).
func Config() config.Config {
	mu.RLock()
	defer mu.RUnlock()
	return cfg
}

// Plan returns the Cline usage poller, or nil when the subscription card is off.
func Plan() *plan.Poller {
	mu.RLock()
	defer mu.RUnlock()
	return pol
}

// Observation returns the channel-observation collector, or nil when it is switched off.
func Observation() *observation.Recorder {
	mu.RLock()
	defer mu.RUnlock()
	return obs
}

// SetConfig publishes a new configuration.
func SetConfig(value config.Config) {
	mu.Lock()
	cfg = value
	mu.Unlock()
}

// SetPlan publishes the poller.
func SetPlan(value *plan.Poller) {
	mu.Lock()
	pol = value
	mu.Unlock()
}

// SetObservation publishes the collector.
func SetObservation(value *observation.Recorder) {
	mu.Lock()
	obs = value
	mu.Unlock()
}

// ChannelLog returns the CPA request-log scanner, or nil when it is switched off.
func ChannelLog() *channellog.Scanner {
	mu.RLock()
	defer mu.RUnlock()
	return clog
}

// SetChannelLog publishes the scanner.
func SetChannelLog(value *channellog.Scanner) {
	mu.Lock()
	clog = value
	mu.Unlock()
}
