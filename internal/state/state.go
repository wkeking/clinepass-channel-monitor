// Package state holds the process-wide runtime state: the active configuration, the ring
// buffer store and the Cline usage poller.
//
// The plugin package publishes it on load and on every reconfigure; the hooks, the plan
// poller and the management API read it. Keeping it in its own package is what lets those
// packages depend on one another without cycles.
package state

import (
	"sync"

	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/plan"
	"github.com/wkeking/clinepass-channel-monitor/internal/store"
)

var (
	mu  sync.RWMutex
	cfg = config.Default()
	st  *store.Store
	pol *plan.Poller
)

// Config returns the active configuration (defaults before the first load).
func Config() config.Config {
	mu.RLock()
	defer mu.RUnlock()
	return cfg
}

// Store returns the active store, or nil before the first load.
func Store() *store.Store {
	mu.RLock()
	defer mu.RUnlock()
	return st
}

// Plan returns the Cline usage poller, or nil when the subscription card is off.
func Plan() *plan.Poller {
	mu.RLock()
	defer mu.RUnlock()
	return pol
}

// SetConfig publishes a new configuration.
func SetConfig(value config.Config) {
	mu.Lock()
	cfg = value
	mu.Unlock()
}

// SetStore publishes the store.
func SetStore(value *store.Store) {
	mu.Lock()
	st = value
	mu.Unlock()
}

// SetPlan publishes the poller.
func SetPlan(value *plan.Poller) {
	mu.Lock()
	pol = value
	mu.Unlock()
}
