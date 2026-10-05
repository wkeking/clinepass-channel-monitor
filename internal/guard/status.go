package guard

import "time"

// Status is the guard's read-only snapshot for /health and the management page. It carries
// account names, channel names, counters and timestamps, and never a credential value.
type Status struct {
	Enabled bool `json:"enabled"`
	// DryRun says the guard is computing decisions without touching CPA's configuration.
	DryRun bool `json:"dry_run"`
	// Judging is false when the guard has no channel data to judge (observation or the
	// request-log scanner is off): the page has to say that instead of looking healthy.
	Judging bool `json:"judging"`
	// Path is the configuration file a decision would rewrite.
	Path string `json:"path,omitempty"`
	// Entries is how many openai-compatibility accounts the last tick saw.
	Entries int `json:"entries"`
	// Threshold and Baseline are the rule the guard is applying, so the page can say
	// "2/3 次" without the reader having to open the configuration.
	Threshold int    `json:"threshold"`
	Baseline  string `json:"baseline,omitempty"`
	// LastTickAt and LastActionAt are UTC.
	LastTickAt   *time.Time `json:"last_tick_at,omitempty"`
	LastActionAt *time.Time `json:"last_action_at,omitempty"`
	// Error is the last tick's failure, empty when the last tick was clean.
	Error string `json:"error,omitempty"`
	// Accounts is one line per configured account, in file order.
	Accounts []StatusAccount `json:"accounts,omitempty"`
	// Recent holds the most recent decisions (newest last, at most ten).
	Recent []Decision `json:"recent,omitempty"`
}

// StatusAccount is one account's line in the snapshot.
type StatusAccount struct {
	Name        string `json:"name"`
	ProviderKey string `json:"provider_key"`
	// Disabled is what the configuration file says.
	Disabled bool `json:"disabled"`
	// ByGuard is true when the guard is the one that disabled this account.
	ByGuard bool `json:"by_guard"`
	// Streak is the current run of consecutive off-baseline channels.
	Streak int `json:"streak"`
	// LastChannel is the most recent channel seen for this account.
	LastChannel string `json:"last_channel,omitempty"`
	// LastSampleAt is when that channel was seen.
	LastSampleAt *time.Time `json:"last_sample_at,omitempty"`
	// NextRetryAt is when the guard will try the account again.
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	// DisableCount is how many times the guard has disabled this account.
	DisableCount int `json:"disable_count"`
	// Pending is true while a written decision has not been read back from the file yet.
	Pending bool `json:"pending,omitempty"`
}
