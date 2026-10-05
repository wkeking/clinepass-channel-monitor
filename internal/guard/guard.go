// Package guard holds the "account guard" decision logic and its persistence.
//
// The guard watches the channel that actually served every recent request. When an
// account's requests are served by a channel other than the configured baseline
// several times in a row, the guard decides that the account should be disabled, and
// after an exponential backoff that it should be enabled again.
//
// The package is deliberately pure logic plus files:
//
//   - Advance consumes one batch of already channel-joined samples and returns the
//     decisions the caller must carry out. It never touches CPA's configuration, the
//     management API or internal/hostconf, and it makes no network calls.
//   - Store keeps the running state in account-guard.json (atomic replace) and appends
//     one JSON line per action to account-guard.jsonl, both under a directory the
//     caller owns.
//
// Unknown providers, samples with no channel block and requests that cannot be joined
// are never counted; guessing a channel would be indistinguishable from measuring it.
package guard

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Defaults applied when the matching Options field carries no usable value.
const (
	defaultThreshold         = 3
	defaultMinEnabled        = 1
	defaultReenableMinutes   = 30
	defaultMaxDisableMinutes = 360
)

// seenLimit bounds how many recent request tokens a State remembers for restart-safe
// deduplication. The caller feeds a bounded window each round, so a fixed ring is
// enough and the persisted state stays small.
const seenLimit = 4096

// channelMemory is how many recent channel names a decision carries for audit.
const channelMemory = 5

// Action strings reported on a Decision.
const (
	ActionDisable    = "disable"
	ActionEnable     = "enable"
	ActionDisableDry = "disable_dry_run"
)

// stateVersion is written into every persisted State so a later version can tell the
// shapes apart.
const stateVersion = 1

// Options configures one Advance call. The zero value is a usable, but inert,
// configuration: Enabled is false and Baseline is empty, so only recovery runs.
type Options struct {
	// Enabled is the master switch. When false, Advance runs only the recovery timer.
	Enabled bool
	// DryRun reports what the guard would do without changing any guarded state.
	DryRun bool
	// Threshold is how many consecutive off-baseline channels trigger a disable.
	// Values <= 0 fall back to 3.
	Threshold int
	// MinEnabled is how many enabled accounts must remain after a disable. Values
	// <= 0 fall back to 1.
	MinEnabled int
	// ScopeNames restricts decisions to these account names, matched case
	// insensitively. Empty means every account.
	ScopeNames []string
	// ReenableMinutes is the first backoff step. Negative values fall back to 30;
	// exactly 0 means the guard never re-enables an account on its own.
	ReenableMinutes int
	// MaxDisableMinutes caps the exponential backoff. Values <= 0 fall back to 360.
	MaxDisableMinutes int
	// Baseline is the channel every other one is compared against, matched case
	// insensitively. Empty disables all judging.
	Baseline string
}

// Account is one account's current state as read from CPA's configuration.
type Account struct {
	// Name is the entry name, e.g. "Cline1".
	Name string
	// ProviderKey is the record's cpa_provider, e.g. "openai-compatible-cline1".
	ProviderKey string
	// Disabled is the entry's current disabled flag.
	Disabled bool
}

// Sample is one "record + joined channel" fed to the guard.
type Sample struct {
	// Time is the record time; samples are expected in ascending time order.
	Time time.Time
	// RequestID identifies the record and is used for restart-safe deduplication.
	RequestID string
	// ProviderKey is the record's cpa_provider.
	ProviderKey string
	// Channel is final_provider. Empty means the record carries no channel block,
	// which neither counts nor resets a streak.
	Channel string
}

// Decision is one action to carry out (or, under dry-run, one action that would have
// been carried out).
type Decision struct {
	Time        time.Time `json:"time"`
	Name        string    `json:"name"`
	ProviderKey string    `json:"provider_key"`
	Action      string    `json:"action"`
	Streak      int       `json:"streak"`
	Baseline    string    `json:"baseline,omitempty"`
	// Channels are the channel names that led to this decision, in ascending time
	// order and at most the five most recent ones.
	Channels []string `json:"channels,omitempty"`
	// Reason is a one-line Chinese summary for the page and the host log.
	Reason string `json:"reason"`
}

// State is the guard's persisted memory: the per-account counters plus a bounded ring
// of already-consumed request tokens. It is JSON serializable; Store owns the file.
type State struct {
	Version  int                      `json:"version"`
	Accounts map[string]*AccountState `json:"accounts"`
	Seen     []string                 `json:"seen,omitempty"`

	// seen mirrors Seen as a set for O(1) lookups. It is rebuilt on load and never
	// serialized.
	seen map[string]struct{}
}

// AccountState is the guard's memory for one account, keyed by provider key.
type AccountState struct {
	Name            string     `json:"name,omitempty"`
	Streak          int        `json:"streak"`
	Channels        []string   `json:"channels,omitempty"`
	DisabledByGuard bool       `json:"disabled_by_guard"`
	DisableCount    int        `json:"disable_count"`
	NextRetryAt     *time.Time `json:"next_retry_at,omitempty"`
	LastChannel     string     `json:"last_channel,omitempty"`
	LastSampleAt    *time.Time `json:"last_sample_at,omitempty"`
}

// NewState returns an empty state ready for Advance.
func NewState() *State {
	return &State{
		Version:  stateVersion,
		Accounts: map[string]*AccountState{},
		seen:     map[string]struct{}{},
	}
}

// Advance consumes one batch of samples and returns this round's decisions. The caller
// is expected to apply them in order and to feed non-overlapping, ascending batches.
func (s *State) Advance(samples []Sample, accounts []Account, now time.Time, opts Options) []Decision {
	opts = opts.normalized()
	unique := uniqueAccounts(accounts)
	index := make(map[string]Account, len(unique))
	for _, account := range unique {
		if key := normalizeKey(account.ProviderKey); key != "" {
			index[key] = account
		}
	}

	// Counting only runs when the guard is allowed to judge at all; recovery below
	// still runs so an account the guard disabled can come back.
	if opts.canJudge() {
		s.count(samples, index, opts)
	}
	return s.decide(unique, now, opts)
}

// canJudge reports whether off-baseline streaks are meaningful this round.
func (o Options) canJudge() bool {
	return o.Enabled && strings.TrimSpace(o.Baseline) != ""
}

// normalized applies the documented defaults. A zero ReenableMinutes is meaningful and
// kept: it means the guard never re-enables an account on its own.
func (o Options) normalized() Options {
	if o.Threshold <= 0 {
		o.Threshold = defaultThreshold
	}
	if o.MinEnabled <= 0 {
		o.MinEnabled = defaultMinEnabled
	}
	if o.ReenableMinutes < 0 {
		o.ReenableMinutes = defaultReenableMinutes
	}
	if o.MaxDisableMinutes <= 0 {
		o.MaxDisableMinutes = defaultMaxDisableMinutes
	}
	return o
}

// scopeSet returns the lowercased scope names, or nil when every account is in scope.
func (o Options) scopeSet() map[string]struct{} {
	if len(o.ScopeNames) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(o.ScopeNames))
	for _, name := range o.ScopeNames {
		if key := normalizeKey(name); key != "" {
			set[key] = struct{}{}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// count folds one batch of samples into the per-account streaks.
func (s *State) count(samples []Sample, index map[string]Account, opts Options) {
	baseline := strings.TrimSpace(opts.Baseline)
	for _, sample := range samples {
		key := normalizeKey(sample.ProviderKey)
		if key == "" {
			continue
		}
		account, known := index[key]
		if !known {
			// Rule: only providers that appear in accounts are counted at all.
			continue
		}
		channel := strings.TrimSpace(sample.Channel)
		if channel == "" {
			// Rule 1: no channel block, neither +1 nor reset - and not consumed either. The
			// channel is read out of CPA's request log, which the scanner only picks up seconds
			// after the request ends (channel_log_min_age_seconds plus a poll), while this runs
			// every few seconds over the whole window. So "no channel" mostly means "the fact has
			// not landed yet", and the same request has to be looked at again next round. Marking
			// it seen here consumed it without counting it: on the production host on 2026-10-05
			// every recent off-baseline request of one account sat in the seen ring while its
			// streak stayed at 1.
			continue
		}
		if s.markSeen(sampleToken(sample, key)) {
			// Consumed in an earlier round (e.g. before a restart): never count twice.
			continue
		}
		state := s.account(key)
		if state.Name == "" {
			state.Name = account.Name
		}
		state.LastChannel = channel
		at := sample.Time
		state.LastSampleAt = &at
		if strings.EqualFold(channel, baseline) {
			// Rule 2: a baseline channel resets the streak.
			state.Streak = 0
			state.Channels = nil
			continue
		}
		// Rule 3: any other channel extends the streak.
		state.Streak++
		state.Channels = appendBounded(state.Channels, channel, channelMemory)
	}
}

// decide reconciles the remembered state with the caller's account snapshot and emits
// at most one decision per account.
func (s *State) decide(accounts []Account, now time.Time, opts Options) []Decision {
	scope := opts.scopeSet()
	inScope := func(name string) bool {
		if scope == nil {
			return true
		}
		_, ok := scope[normalizeKey(name)]
		return ok
	}

	// Rule 12: MinEnabled counts only in-scope accounts.
	enabledInScope := 0
	for _, account := range accounts {
		if inScope(account.Name) && !account.Disabled {
			enabledInScope++
		}
	}

	var decisions []Decision
	disables := 0
	for _, account := range accounts {
		key := normalizeKey(account.ProviderKey)
		if key == "" {
			continue
		}
		state := s.account(key)
		state.Name = account.Name

		switch {
		case account.Disabled && state.DisabledByGuard:
			// Rules 8/10: an account the guard disabled comes back once the backoff
			// has elapsed, whatever the master switch and baseline are.
			if state.NextRetryAt == nil || now.Before(*state.NextRetryAt) {
				continue
			}
			decisions = append(decisions, Decision{
				Time:        now,
				Name:        account.Name,
				ProviderKey: account.ProviderKey,
				Action:      ActionEnable,
				Streak:      state.Streak,
				Baseline:    strings.TrimSpace(opts.Baseline),
				Channels:    append([]string(nil), state.Channels...),
				Reason:      fmt.Sprintf("退避到期（第 %d 次禁用后），恢复该账号", state.DisableCount),
			})
			state.DisabledByGuard = false
			state.Streak = 0
			state.Channels = nil
			state.NextRetryAt = nil
			// DisableCount is kept so the next offence escalates the backoff.

		case !account.Disabled && state.DisabledByGuard:
			// Rule 9a: enabled again by hand. The operator wins: forget everything the
			// guard remembered and take no action this round.
			state.DisabledByGuard = false
			state.Streak = 0
			state.Channels = nil
			state.DisableCount = 0
			state.NextRetryAt = nil

		case account.Disabled && !state.DisabledByGuard:
			// Rule 9b: disabled by someone else. Do not keep counting towards a
			// disable we are not responsible for.
			state.Streak = 0
			state.Channels = nil

		default:
			if !opts.canJudge() || !inScope(account.Name) || state.Streak < opts.Threshold {
				continue
			}
			if opts.DryRun {
				// Rule 5: report once per streak, at the exact threshold step, and
				// change nothing.
				if state.Streak != opts.Threshold {
					continue
				}
				decisions = append(decisions, Decision{
					Time:        now,
					Name:        account.Name,
					ProviderKey: account.ProviderKey,
					Action:      ActionDisableDry,
					Streak:      state.Streak,
					Baseline:    strings.TrimSpace(opts.Baseline),
					Channels:    append([]string(nil), state.Channels...),
					Reason: fmt.Sprintf(
						"dry-run：连续 %d 次真实渠道不是基准渠道 %s，本应禁用该账号",
						state.Streak, strings.TrimSpace(opts.Baseline)),
				})
				continue
			}
			// Rule 4: keep at least MinEnabled accounts enabled. When blocked, the
			// streak keeps growing and a later round can trigger instead.
			if enabledInScope-disables <= opts.MinEnabled {
				continue
			}
			disables++
			state.DisabledByGuard = true
			state.DisableCount++
			state.NextRetryAt = backoffNext(now, state.DisableCount, opts)
			decisions = append(decisions, Decision{
				Time:        now,
				Name:        account.Name,
				ProviderKey: account.ProviderKey,
				Action:      ActionDisable,
				Streak:      state.Streak,
				Baseline:    strings.TrimSpace(opts.Baseline),
				Channels:    append([]string(nil), state.Channels...),
				Reason: fmt.Sprintf(
					"连续 %d 次真实渠道不是基准渠道 %s，禁用该账号",
					state.Streak, strings.TrimSpace(opts.Baseline)),
			})
		}
	}
	return decisions
}

// backoffNext returns now plus the next backoff step, or nil when the guard never
// re-enables on its own.
func backoffNext(now time.Time, disableCount int, opts Options) *time.Time {
	if opts.ReenableMinutes == 0 {
		return nil
	}
	minutes := float64(opts.ReenableMinutes) * math.Pow(2, float64(disableCount-1))
	if minutes > float64(opts.MaxDisableMinutes) {
		minutes = float64(opts.MaxDisableMinutes)
	}
	next := now.Add(time.Duration(minutes * float64(time.Minute)))
	return &next
}

// account returns the memory slot for a provider key, creating it on demand.
func (s *State) account(key string) *AccountState {
	if s.Accounts == nil {
		s.Accounts = map[string]*AccountState{}
	}
	state, ok := s.Accounts[key]
	if !ok || state == nil {
		state = &AccountState{}
		s.Accounts[key] = state
	}
	return state
}

// markSeen records a request token and reports whether it had been seen before.
func (s *State) markSeen(token string) bool {
	if s.seen == nil {
		s.hydrate()
	}
	if _, ok := s.seen[token]; ok {
		return true
	}
	s.seen[token] = struct{}{}
	s.Seen = append(s.Seen, token)
	if len(s.Seen) > seenLimit {
		drop := len(s.Seen) - seenLimit
		for _, stale := range s.Seen[:drop] {
			delete(s.seen, stale)
		}
		s.Seen = append([]string(nil), s.Seen[drop:]...)
	}
	return false
}

// hydrate rebuilds the derived structures the state keeps next to its JSON fields.
func (s *State) hydrate() {
	if s.Accounts == nil {
		s.Accounts = map[string]*AccountState{}
	}
	s.seen = make(map[string]struct{}, len(s.Seen))
	for _, token := range s.Seen {
		s.seen[token] = struct{}{}
	}
	if s.Version == 0 {
		s.Version = stateVersion
	}
}

// sampleToken is the deduplication identity of one sample. A record id is the natural
// identity; a sample without one falls back to provider plus timestamp.
func sampleToken(sample Sample, key string) string {
	if id := strings.TrimSpace(sample.RequestID); id != "" {
		return key + "|" + id
	}
	return key + "@" + sample.Time.UTC().Format(time.RFC3339Nano)
}

// normalizeKey lowercases and trims a provider key or account name for comparison.
func normalizeKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// uniqueAccounts keeps the first entry per provider key so one account can never emit
// two decisions in a single round.
func uniqueAccounts(accounts []Account) []Account {
	seen := make(map[string]struct{}, len(accounts))
	out := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		key := normalizeKey(account.ProviderKey)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, account)
	}
	return out
}

// appendBounded appends value and keeps at most limit entries, dropping the oldest.
func appendBounded(list []string, value string, limit int) []string {
	list = append(list, value)
	if len(list) > limit {
		list = append([]string(nil), list[len(list)-limit:]...)
	}
	return list
}
