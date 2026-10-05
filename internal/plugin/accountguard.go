package plugin

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
	"github.com/wkeking/clinepass-channel-monitor/internal/config"
	"github.com/wkeking/clinepass-channel-monitor/internal/guard"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostapi"
	"github.com/wkeking/clinepass-channel-monitor/internal/hostconf"
	"github.com/wkeking/clinepass-channel-monitor/internal/observation"
	"github.com/wkeking/clinepass-channel-monitor/internal/state"
)

// The account guard is the one part of this plugin that writes to CPA's own configuration: when
// an account's gateway channel stays off the baseline for account_guard_threshold requests in a
// row, it flips that openai-compatibility entry's `disabled` (hostconf.SetEntryDisabled, which
// touches one token and nothing else) and later flips it back on the backoff schedule.
//
// Three properties make that safe enough to run on somebody's gateway:
//
//   - the decision comes from the same join the page renders (Recorder.ChannelRecordsSince), so
//     the guard can never disagree with the table above it about what a request's channel was;
//   - the state is written before the configuration is, so the plugin's own reload - which the
//     configuration write triggers - cannot forget that the guard is the one who disabled the
//     account;
//   - a write that is not read back from the file is treated as not done: the runner keeps
//     reporting the wanted value (pending) instead of letting guard.Advance read a stale
//     "enabled" as "the operator changed it back".
const (
	// accountGuardTick is how often the guard reconciles. The channel scanner polls every two
	// seconds, so five keeps a three-request streak within a few seconds of real time without
	// re-reading the day's JSONL any more often than it has to.
	accountGuardTick = 5 * time.Second
	// accountGuardWindow is how much record history one tick judges. Every sample is deduplicated
	// by request id inside the guard, so re-reading the same hour is free; one hour is simply the
	// smallest window that cannot miss a request between two ticks.
	accountGuardWindow = observation.Window1h
	// accountGuardSaveEvery bounds how much evidence a plugin reload can re-count: only samples
	// consumed in the last few seconds are not in the state file yet.
	accountGuardSaveEvery = 15 * time.Second
	// accountGuardRecent is how many past decisions /health keeps for the page.
	accountGuardRecent = 10
)

var (
	accountGuardMu     sync.Mutex
	accountGuardActive *accountGuardRunner
)

// accountGuardRunner owns the ticker, the guard's state file and the published snapshot.
type accountGuardRunner struct {
	cfg   config.Config
	store *guard.Store
	stop  chan struct{}
	done  chan struct{}

	mu       sync.Mutex
	state    *guard.State
	status   guard.Status
	recent   []guard.Decision
	pending  map[string]bool
	lastSave time.Time
}

// startAccountGuard publishes the guard and starts its loop. It runs even when the guard is
// switched off: the page and /health then show "switched off" instead of nothing, and the
// recovery timer for an account a previous run disabled keeps running.
func startAccountGuard(cfg config.Config) {
	runner := &accountGuardRunner{
		cfg:     cfg,
		store:   guard.NewStore(cfg.ChannelStoreDir),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		pending: map[string]bool{},
	}
	loaded, errLoad := runner.store.Load()
	if errLoad != nil {
		hostapi.LogAsync("warn", buildinfo.ID+": account guard state unreadable, starting empty", map[string]string{
			"error": errLoad.Error(),
		})
		loaded = guard.NewState()
	}
	runner.state = loaded
	// One synchronous tick: the snapshot exists the moment the configuration is applied, so a
	// reload never leaves the page without a guard block.
	runner.tick()
	accountGuardMu.Lock()
	accountGuardActive = runner
	accountGuardMu.Unlock()
	go runner.run()
}

// stopAccountGuard stops the loop and unpublishes the snapshot.
func stopAccountGuard() {
	accountGuardMu.Lock()
	runner := accountGuardActive
	accountGuardActive = nil
	accountGuardMu.Unlock()
	state.SetAccountGuard(nil)
	if runner != nil {
		close(runner.stop)
		<-runner.done
	}
}

func (r *accountGuardRunner) run() {
	defer close(r.done)
	ticker := time.NewTicker(accountGuardTick)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			r.tick()
		}
	}
}

// tick is one reconciliation: read the accounts, judge the records, carry out what the guard
// decided, then publish.
func (r *accountGuardRunner) tick() {
	now := time.Now().UTC()
	path, raw, errRead := hostconf.ReadRaw(r.cfg)
	if errRead != nil {
		r.publish(path, nil, false, errRead.Error(), now)
		return
	}
	accounts := r.accounts(raw)
	samples := r.samples()
	decisions := r.state.Advance(samples, accounts, now, r.options())
	for _, decision := range decisions {
		r.record(decision)
		r.apply(decision)
	}
	r.publish(path, accounts, r.judging(samples), "", now)
	r.maybeSave(now, len(decisions) > 0)
}

// accounts reads the guard's view of the configuration: what the file says, with any write this
// runner is still waiting to read back applied on top (see accountGuardRunner.pending).
func (r *accountGuardRunner) accounts(raw []byte) []guard.Account {
	entries := hostconf.Entries(raw)
	accounts := make([]guard.Account, 0, len(entries))
	r.mu.Lock()
	pending := make(map[string]bool, len(r.pending))
	for key, value := range r.pending {
		pending[key] = value
	}
	r.mu.Unlock()
	for _, entry := range entries {
		disabled := entry.Disabled
		if wanted, ok := pending[entry.ProviderKey]; ok {
			disabled = wanted
		}
		accounts = append(accounts, guard.Account{
			Name:        entry.Name,
			ProviderKey: entry.ProviderKey,
			Disabled:    disabled,
		})
	}
	return accounts
}

// samples is the last hour of records with their gateway channel joined in, oldest first. The
// join is the recorder's own, so the guard and the page always agree about a channel.
func (r *accountGuardRunner) samples() []guard.Sample {
	if !r.cfg.AccountGuardEnabled {
		return nil
	}
	recorder := state.Observation()
	if recorder == nil {
		return nil
	}
	records, errLoad := recorder.ChannelRecordsSince(accountGuardWindow)
	if errLoad != nil {
		hostapi.LogAsync("warn", buildinfo.ID+": account guard could not read the channel records", map[string]string{
			"error": errLoad.Error(),
		})
	}
	return guardSamples(records)
}

// guardSamples maps the page's joined records onto the state machine's input: the credential CPA
// routed to, the gateway channel the join found (empty when it found none) and the record's own
// identity, which is what makes re-reading the same hour safe. Records come back newest first and
// the state machine reads a streak in time order, so this sorts them oldest first and drops the
// records that carry no credential at all (nothing to attribute them to).
func guardSamples(records []observation.Record) []guard.Sample {
	samples := make([]guard.Sample, 0, len(records))
	for _, record := range records {
		if record.CPProvider == "" {
			continue
		}
		samples = append(samples, guard.Sample{
			Time:        record.Time,
			RequestID:   record.RequestID,
			ProviderKey: record.CPProvider,
			Channel:     record.FinalProvider,
		})
	}
	sort.SliceStable(samples, func(first, second int) bool {
		return samples[first].Time.Before(samples[second].Time)
	})
	return samples
}

// judging reports whether this tick had channel data to judge at all.
func (r *accountGuardRunner) judging(samples []guard.Sample) bool {
	return r.cfg.AccountGuardEnabled && len(samples) > 0
}

func (r *accountGuardRunner) options() guard.Options {
	return guard.Options{
		Enabled:           r.cfg.AccountGuardEnabled,
		DryRun:            r.cfg.AccountGuardDryRun,
		Threshold:         r.cfg.AccountGuardThreshold,
		MinEnabled:        r.cfg.AccountGuardMinEnabled,
		ScopeNames:        r.cfg.AccountGuardScopeNames,
		ReenableMinutes:   r.cfg.AccountGuardReenableMinutes,
		MaxDisableMinutes: r.cfg.AccountGuardMaxDisableMinutes,
		Baseline:          r.cfg.ChannelBaselineProvider,
	}
}

// record appends one decision to the audit file and to the page's short history. The audit line
// is written before the action: a decision that cannot be recorded is a decision that must not
// be carried out.
func (r *accountGuardRunner) record(decision guard.Decision) {
	if errAudit := r.store.AppendAudit(decision); errAudit != nil {
		hostapi.LogAsync("warn", buildinfo.ID+": account guard could not write its audit line", map[string]string{
			"error":  errAudit.Error(),
			"action": decision.Action,
			"entry":  decision.Name,
		})
	}
	r.mu.Lock()
	r.recent = append(r.recent, decision)
	if len(r.recent) > accountGuardRecent {
		r.recent = append([]guard.Decision(nil), r.recent[len(r.recent)-accountGuardRecent:]...)
	}
	r.mu.Unlock()
}

// apply carries one decision out. Every failure keeps the wanted value in `pending`, so the next
// tick keeps showing it to the state machine instead of reading "still enabled" as an operator
// override.
func (r *accountGuardRunner) apply(decision guard.Decision) {
	if decision.Action == "disable_dry_run" {
		hostapi.LogAsync("info", buildinfo.ID+": account guard (dry run) "+decision.Reason, nil)
		return
	}
	want := decision.Action == "disable"
	path, raw, errRead := hostconf.ReadRaw(r.cfg)
	if errRead != nil {
		r.fail(decision, want, "read configuration: "+errRead.Error())
		return
	}
	updated, changed, errSet := hostconf.SetEntryDisabled(raw, decision.Name, want)
	if errSet != nil {
		r.fail(decision, want, "rewrite configuration: "+errSet.Error())
		return
	}
	if !changed {
		// The file already says what the decision wants. Nothing to write, nothing to remember.
		r.clearPending(decision.ProviderKey)
		return
	}
	if errState := r.store.Save(r.state); errState != nil {
		hostapi.LogAsync("warn", buildinfo.ID+": account guard state not saved before the write", map[string]string{
			"error": errState.Error(),
			"entry": decision.Name,
		})
	}
	if errWrite := writeConfigAtomic(path, updated); errWrite != nil {
		r.fail(decision, want, "write configuration: "+errWrite.Error())
		return
	}
	_, verified, errVerify := hostconf.ReadRaw(r.cfg)
	if errVerify != nil || !entryDisabled(verified, decision.Name, want) {
		r.fail(decision, want, "configuration did not read back")
		return
	}
	r.clearPending(decision.ProviderKey)
	r.mu.Lock()
	now := time.Now().UTC()
	r.status.LastActionAt = &now
	r.mu.Unlock()
	hostapi.LogAsync("info", buildinfo.ID+": account guard "+decision.Reason, map[string]string{
		"entry":  decision.Name,
		"action": decision.Action,
		"path":   path,
	})
}

// fail logs a write that did not land and keeps the wanted value pending.
func (r *accountGuardRunner) fail(decision guard.Decision, want bool, message string) {
	r.mu.Lock()
	r.pending[decision.ProviderKey] = want
	r.mu.Unlock()
	hostapi.LogAsync("warn", buildinfo.ID+": account guard could not "+decision.Action+" "+decision.Name, map[string]string{
		"error": message,
		"entry": decision.Name,
	})
}

func (r *accountGuardRunner) clearPending(providerKey string) {
	r.mu.Lock()
	delete(r.pending, providerKey)
	r.mu.Unlock()
}

// publish builds the snapshot the page reads. It is called after every tick, including the ticks
// that failed, so an operator sees the failure instead of a stale healthy state.
func (r *accountGuardRunner) publish(path string, accounts []guard.Account, judging bool, tickError string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := guard.Status{
		Enabled:    r.cfg.AccountGuardEnabled,
		DryRun:     r.cfg.AccountGuardDryRun,
		Judging:    judging,
		Path:       path,
		Entries:    len(accounts),
		Threshold:  r.cfg.AccountGuardThreshold,
		Baseline:   r.cfg.ChannelBaselineProvider,
		LastTickAt: &now,
		Error:      tickError,
		Recent:     append([]guard.Decision(nil), r.recent...),
	}
	if r.status.LastActionAt != nil {
		lastAction := *r.status.LastActionAt
		status.LastActionAt = &lastAction
	}
	status.Accounts = make([]guard.StatusAccount, 0, len(accounts))
	for _, account := range accounts {
		line := guard.StatusAccount{
			Name:        account.Name,
			ProviderKey: account.ProviderKey,
			Disabled:    account.Disabled,
			Pending:     r.pending[account.ProviderKey],
		}
		if remembered, ok := r.state.Accounts[account.ProviderKey]; ok && remembered != nil {
			line.ByGuard = remembered.DisabledByGuard
			line.Streak = remembered.Streak
			line.LastChannel = remembered.LastChannel
			line.LastSampleAt = remembered.LastSampleAt
			line.NextRetryAt = remembered.NextRetryAt
			line.DisableCount = remembered.DisableCount
		}
		status.Accounts = append(status.Accounts, line)
	}
	r.status = status
	snapshot := status
	snapshot.Recent = append([]guard.Decision(nil), status.Recent...)
	snapshot.Accounts = append([]guard.StatusAccount(nil), status.Accounts...)
	state.SetAccountGuard(&snapshot)
}

// maybeSave checkpoints the state. It is written after a decision and otherwise every
// accountGuardSaveEvery, which bounds what a reload can re-count to a few seconds of samples.
func (r *accountGuardRunner) maybeSave(now time.Time, forced bool) {
	r.mu.Lock()
	due := forced || now.Sub(r.lastSave) >= accountGuardSaveEvery
	if due {
		r.lastSave = now
	}
	r.mu.Unlock()
	if !due {
		return
	}
	if errSave := r.store.Save(r.state); errSave != nil {
		hostapi.LogAsync("warn", buildinfo.ID+": account guard state not saved", map[string]string{
			"error": errSave.Error(),
		})
	}
}

// entryDisabled reports what the file now says about one entry.
func entryDisabled(raw []byte, name string, want bool) bool {
	for _, entry := range hostconf.Entries(raw) {
		if entry.Name == name {
			return entry.Disabled == want
		}
	}
	return false
}

// writeConfigAtomic replaces CPA's configuration file in one rename, keeping its permissions.
// The plugin never writes that file any other way: a half-written file would be read by the host
// watcher and could take the gateway down with it. Both writers - the account guard and the
// configuration-defaults seeder - go through here.
func writeConfigAtomic(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if info, errStat := os.Stat(path); errStat == nil {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, errCreate := os.CreateTemp(dir, ".clinepass-*.tmp")
	if errCreate != nil {
		return errCreate
	}
	name := tmp.Name()
	defer func() {
		_ = os.Remove(name)
	}()
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		return errWrite
	}
	if errSync := tmp.Sync(); errSync != nil {
		_ = tmp.Close()
		return errSync
	}
	if errClose := tmp.Close(); errClose != nil {
		return errClose
	}
	if errChmod := os.Chmod(name, mode); errChmod != nil {
		return errChmod
	}
	return os.Rename(name, path)
}

// accountGuardLogFields is the one-line summary a reconfigure writes into the host log.
func accountGuardLogFields(cfg config.Config) map[string]string {
	return map[string]string{
		"account_guard":           strconv.FormatBool(cfg.AccountGuardEnabled),
		"account_guard_dry_run":   strconv.FormatBool(cfg.AccountGuardDryRun),
		"account_guard_threshold": strconv.Itoa(cfg.AccountGuardThreshold),
	}
}
