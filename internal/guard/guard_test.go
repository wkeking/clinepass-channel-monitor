package guard

import (
	"strconv"
	"testing"
	"time"
)

const (
	cline1Key = "openai-compatible-cline1"
	cline2Key = "openai-compatible-cline2"
	cline3Key = "openai-compatible-cline3"
	ghostKey  = "openai-compatible-ghost"
)

var guardBase = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

// at returns the guard clock offset by n seconds from the fixed test base.
func at(n int) time.Time { return guardBase.Add(time.Duration(n) * time.Second) }

// smp builds one joined sample.
func smp(sec int, id, provider, channel string) Sample {
	return Sample{Time: at(sec), RequestID: id, ProviderKey: provider, Channel: channel}
}

// acct builds one account snapshot.
func acct(name, key string, disabled bool) Account {
	return Account{Name: name, ProviderKey: key, Disabled: disabled}
}

// witness is a second, always-enabled account. It keeps a disable legal under the
// default MinEnabled of 1 when a test only exercises one guarded account.
func witness() Account { return acct("Cline2", cline2Key, false) }

// judgeOpts is the fully specified options most tests use.
func judgeOpts() Options {
	return Options{
		Enabled:           true,
		Threshold:         3,
		MinEnabled:        1,
		ReenableMinutes:   30,
		MaxDisableMinutes: 360,
		Baseline:          "deepseek",
	}
}

// slotFor returns an account's state slot, failing the test when it is missing.
func slotFor(t *testing.T, state *State, key string) *AccountState {
	t.Helper()
	slot, ok := state.Accounts[key]
	if !ok || slot == nil {
		t.Fatalf("no state slot for %s", key)
	}
	return slot
}

// TestRule1EmptyChannelIgnored covers rule 1: a missing channel block neither counts
// nor resets the streak.
func TestRule1EmptyChannelIgnored(t *testing.T) {
	state := NewState()
	accounts := []Account{acct("Cline1", cline1Key, false), witness()}

	decisions := state.Advance([]Sample{
		smp(0, "r1", cline1Key, ""),
		smp(1, "r2", cline1Key, "   "),
		smp(2, "r3", cline1Key, ""),
	}, accounts, at(3), judgeOpts())
	if len(decisions) != 0 {
		t.Fatalf("empty channels produced decisions: %+v", decisions)
	}
	if got := slotFor(t, state, cline1Key).Streak; got != 0 {
		t.Fatalf("streak after empty channels = %d, want 0", got)
	}

	decisions = state.Advance([]Sample{
		smp(3, "r4", cline1Key, "fireworks"),
		smp(4, "r5", cline1Key, "fireworks"),
		smp(5, "r6", cline1Key, "fireworks"),
	}, accounts, at(6), judgeOpts())
	if len(decisions) != 1 || decisions[0].Action != ActionDisable || decisions[0].Streak != 3 {
		t.Fatalf("first real streak = %+v, want one disable at streak 3", decisions)
	}
}

// TestAJoiningChannelIsCountedWhenTheFactLands pins the defect the production host showed on
// 2026-10-05: an account had four off-baseline requests inside the hour, all four were in the seen
// ring, and its streak was still 1. The cause is the order of the two checks below - the record
// exists the moment CPA's usage hook fires, but its channel only exists after the request log has
// been written and parsed, so the first round sees the record with an empty channel. Consuming it
// there loses the request for good; the same request has to be looked at again once the fact lands.
func TestAJoiningChannelIsCountedWhenTheFactLands(t *testing.T) {
	state := NewState()
	accounts := []Account{acct("Cline1", cline1Key, false), witness()}

	// Round 1: the record exists, the fact has not landed yet.
	state.Advance([]Sample{smp(0, "r1", cline1Key, "")}, accounts, at(1), judgeOpts())
	if got := slotFor(t, state, cline1Key).Streak; got != 0 {
		t.Fatalf("streak with an empty channel = %d, want 0", got)
	}

	// Round 2: the same request, now with its channel. It has to count exactly once.
	joined := []Sample{smp(0, "r1", cline1Key, "openai-compatible-private")}
	if decisions := state.Advance(joined, accounts, at(2), judgeOpts()); len(decisions) != 0 {
		t.Fatalf("decisions = %+v, want none below the threshold", decisions)
	}
	if got := slotFor(t, state, cline1Key).Streak; got != 1 {
		t.Fatalf("streak after the channel landed = %d, want 1", got)
	}

	// Round 3: re-reading the same joined sample must not count it twice.
	state.Advance(joined, accounts, at(3), judgeOpts())
	if got := slotFor(t, state, cline1Key).Streak; got != 1 {
		t.Fatalf("streak after re-reading the same request = %d, want 1", got)
	}

	// Two more off-baseline requests reach the threshold, with the first still counted once.
	decisions := state.Advance([]Sample{
		joined[0],
		smp(1, "r2", cline1Key, "openai-compatible-private"),
		smp(2, "r3", cline1Key, "openai-compatible-private"),
	}, accounts, at(4), judgeOpts())
	if len(decisions) != 1 || decisions[0].Streak != 3 || decisions[0].Action != ActionDisable {
		t.Fatalf("decisions = %+v, want one disable at streak 3", decisions)
	}
}

// TestRule2BaselineResetsCaseInsensitive covers rule 2.
func TestRule2BaselineResetsCaseInsensitive(t *testing.T) {
	state := NewState()
	accounts := []Account{acct("Cline1", cline1Key, false), witness()}

	state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
	}, accounts, at(2), judgeOpts())
	if got := slotFor(t, state, cline1Key).Streak; got != 2 {
		t.Fatalf("streak = %d, want 2", got)
	}

	state.Advance([]Sample{smp(2, "r3", cline1Key, "DeepSeek")}, accounts, at(3), judgeOpts())
	if got := slotFor(t, state, cline1Key).Streak; got != 0 {
		t.Fatalf("streak after baseline = %d, want 0", got)
	}

	decisions := state.Advance([]Sample{
		smp(3, "r4", cline1Key, "fireworks"),
		smp(4, "r5", cline1Key, "fireworks"),
	}, accounts, at(5), judgeOpts())
	if len(decisions) != 0 {
		t.Fatalf("two off-baseline channels after a reset produced %+v", decisions)
	}
	decisions = state.Advance([]Sample{smp(5, "r6", cline1Key, "FIREWORKS")}, accounts, at(6), judgeOpts())
	if len(decisions) != 1 || decisions[0].Streak != 3 {
		t.Fatalf("decision = %+v, want one disable at streak 3", decisions)
	}
}

// TestRule3UnknownProviderIgnored covers rule 3: only providers present in accounts
// are counted.
func TestRule3UnknownProviderIgnored(t *testing.T) {
	state := NewState()
	accounts := []Account{acct("Cline1", cline1Key, false)}

	decisions := state.Advance([]Sample{
		smp(0, "r1", ghostKey, "fireworks"),
		smp(1, "r2", ghostKey, "fireworks"),
		smp(2, "r3", ghostKey, "fireworks"),
		smp(3, "r4", ghostKey, "fireworks"),
	}, accounts, at(4), judgeOpts())
	if len(decisions) != 0 {
		t.Fatalf("unknown provider produced decisions: %+v", decisions)
	}
	if _, ok := state.Accounts[ghostKey]; ok {
		t.Fatalf("unknown provider was tracked: %+v", state.Accounts)
	}
}

// TestRule4ThresholdMinEnabled covers rule 4: the floor keeps MinEnabled accounts
// enabled, and a blocked streak can still trigger in a later round.
func TestRule4ThresholdMinEnabled(t *testing.T) {
	state := NewState()
	accounts := []Account{
		acct("Cline1", cline1Key, false),
		acct("Cline2", cline2Key, false),
		acct("Cline3", cline3Key, false),
	}
	decisions := state.Advance([]Sample{
		smp(0, "a1", cline1Key, "fireworks"),
		smp(1, "a2", cline1Key, "fireworks"),
		smp(2, "a3", cline1Key, "fireworks"),
		smp(3, "b1", cline2Key, "fireworks"),
		smp(4, "b2", cline2Key, "fireworks"),
		smp(5, "b3", cline2Key, "fireworks"),
		smp(6, "c1", cline3Key, "fireworks"),
		smp(7, "c2", cline3Key, "fireworks"),
		smp(8, "c3", cline3Key, "fireworks"),
	}, accounts, at(9), judgeOpts())
	if len(decisions) != 2 {
		t.Fatalf("decisions = %+v, want exactly two disables", decisions)
	}
	if decisions[0].Name != "Cline1" || decisions[1].Name != "Cline2" {
		t.Fatalf("disabled the wrong accounts: %+v", decisions)
	}
	if !slotFor(t, state, cline1Key).DisabledByGuard {
		t.Fatalf("Cline1 not marked as disabled by guard")
	}
	if slot := slotFor(t, state, cline3Key); slot.DisabledByGuard || slot.Streak != 3 {
		t.Fatalf("Cline3 slot = %+v, want an untouched streak of 3", slot)
	}

}

// TestRule4BlockedStreakTriggersLater covers rule 4's "keep counting, act later"
// clause: a streak blocked by the floor fires once more accounts are enabled.
func TestRule4BlockedStreakTriggersLater(t *testing.T) {
	state := NewState()
	decisions := state.Advance([]Sample{
		smp(0, "a1", cline1Key, "fireworks"),
		smp(1, "a2", cline1Key, "fireworks"),
		smp(2, "a3", cline1Key, "fireworks"),
		smp(3, "b1", cline2Key, "fireworks"),
		smp(4, "b2", cline2Key, "fireworks"),
		smp(5, "b3", cline2Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false), acct("Cline2", cline2Key, false)}, at(6), judgeOpts())
	if len(decisions) != 1 || decisions[0].Name != "Cline1" {
		t.Fatalf("first round = %+v, want only Cline1 disabled", decisions)
	}
	if slot := slotFor(t, state, cline2Key); slot.Streak != 3 || slot.DisabledByGuard {
		t.Fatalf("Cline2 slot = %+v, want a surviving streak of 3", slot)
	}

	// Cline1's write landed and a third account joins: now the floor allows Cline2.
	decisions = state.Advance(nil, []Account{
		acct("Cline1", cline1Key, true),
		acct("Cline2", cline2Key, false),
		acct("Cline3", cline3Key, false),
	}, at(20), judgeOpts())
	if len(decisions) != 1 || decisions[0].Name != "Cline2" {
		t.Fatalf("blocked streak did not trigger later: %+v", decisions)
	}
}

// TestRule4MinEnabledBlocksEverything covers the floor: with a single in-scope enabled
// account and MinEnabled 1, nothing is ever disabled.
func TestRule4MinEnabledBlocksEverything(t *testing.T) {
	state := NewState()
	decisions := state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
		smp(2, "r3", cline1Key, "fireworks"),
		smp(3, "r4", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false)}, at(4), judgeOpts())
	if len(decisions) != 0 {
		t.Fatalf("MinEnabled floor was ignored: %+v", decisions)
	}
	if slot := slotFor(t, state, cline1Key); slot.Streak != 4 || slot.DisabledByGuard {
		t.Fatalf("blocked account = %+v, want streak 4 and not disabled", slot)
	}
}

// TestRule5DryRunOnce covers rule 5: dry-run reports once per streak and mutates
// nothing.
func TestRule5DryRunOnce(t *testing.T) {
	opts := judgeOpts()
	opts.DryRun = true
	state := NewState()
	accounts := []Account{acct("Cline1", cline1Key, false)}

	decisions := state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
		smp(2, "r3", cline1Key, "fireworks"),
	}, accounts, at(3), opts)
	if len(decisions) != 1 || decisions[0].Action != ActionDisableDry || decisions[0].Streak != 3 {
		t.Fatalf("dry-run decisions = %+v, want one disable_dry_run at streak 3", decisions)
	}
	slot := slotFor(t, state, cline1Key)
	if slot.DisabledByGuard || slot.DisableCount != 0 || slot.NextRetryAt != nil {
		t.Fatalf("dry-run mutated guarded state: %+v", slot)
	}
	if slot.Streak != 3 {
		t.Fatalf("dry-run streak = %d, want 3", slot.Streak)
	}

	// A fourth off-baseline channel extends the same streak without a second report.
	decisions = state.Advance([]Sample{smp(3, "r4", cline1Key, "fireworks")}, accounts, at(4), opts)
	if len(decisions) != 0 {
		t.Fatalf("dry-run reported twice in one streak: %+v", decisions)
	}

	// A baseline resets the streak, so the next threshold hit reports again.
	state.Advance([]Sample{smp(4, "r5", cline1Key, "deepseek")}, accounts, at(5), opts)
	decisions = state.Advance([]Sample{
		smp(5, "r6", cline1Key, "fireworks"),
		smp(6, "r7", cline1Key, "fireworks"),
		smp(7, "r8", cline1Key, "fireworks"),
	}, accounts, at(8), opts)
	if len(decisions) != 1 || decisions[0].Action != ActionDisableDry {
		t.Fatalf("dry-run did not report after a reset: %+v", decisions)
	}
}

// TestRule5DryRunDoesNotConsultMinEnabled documents that dry-run follows the literal
// rule: it reports what a disable would look like even when the floor would block it.
func TestRule5DryRunDoesNotConsultMinEnabled(t *testing.T) {
	opts := judgeOpts()
	opts.DryRun = true
	state := NewState()
	decisions := state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
		smp(2, "r3", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false)}, at(3), opts)
	if len(decisions) != 1 || decisions[0].Action != ActionDisableDry {
		t.Fatalf("dry-run decisions = %+v, want one report", decisions)
	}
}

// TestRule6OneDecisionPerAccount covers rule 6.
func TestRule6OneDecisionPerAccount(t *testing.T) {
	state := NewState()
	var samples []Sample
	for i := 0; i < 12; i++ {
		samples = append(samples, smp(i, "long-"+strconv.Itoa(i), cline1Key, "fireworks"))
	}
	decisions := state.Advance(samples, []Account{acct("Cline1", cline1Key, false), witness()}, at(20), judgeOpts())
	if len(decisions) != 1 || decisions[0].Streak != 12 {
		t.Fatalf("decisions = %+v, want exactly one at streak 12", decisions)
	}
}

// TestRule7DisableBookkeeping covers rule 7: the guard's memory and the first backoff.
func TestRule7DisableBookkeeping(t *testing.T) {
	state := NewState()
	now := at(30)
	decisions := state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
		smp(2, "r3", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false), witness()}, now, judgeOpts())
	if len(decisions) != 1 || decisions[0].Action != ActionDisable {
		t.Fatalf("decisions = %+v, want a disable", decisions)
	}
	slot := slotFor(t, state, cline1Key)
	if !slot.DisabledByGuard || slot.DisableCount != 1 {
		t.Fatalf("slot = %+v, want disabled_by_guard and disable_count 1", slot)
	}
	if slot.NextRetryAt == nil || !slot.NextRetryAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("NextRetryAt = %v, want %v", slot.NextRetryAt, now.Add(30*time.Minute))
	}

	// ReenableMinutes == 0 means the guard never re-enables on its own.
	never := NewState()
	neverOpts := judgeOpts()
	neverOpts.ReenableMinutes = 0
	never.Advance([]Sample{
		smp(0, "n1", cline1Key, "fireworks"),
		smp(1, "n2", cline1Key, "fireworks"),
		smp(2, "n3", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false), witness()}, now, neverOpts)
	if slot := slotFor(t, never, cline1Key); slot.NextRetryAt != nil {
		t.Fatalf("ReenableMinutes 0 set NextRetryAt = %v, want nil", slot.NextRetryAt)
	}
}

// TestRule8Recovery covers rule 8: the backoff elapses and the guard re-enables.
func TestRule8Recovery(t *testing.T) {
	state := NewState()
	state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
		smp(2, "r3", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false), witness()}, at(0), judgeOpts())
	retryAt := *slotFor(t, state, cline1Key).NextRetryAt
	disabled := []Account{acct("Cline1", cline1Key, true), witness()}

	// Before the retry is due nothing happens.
	if decisions := state.Advance(nil, disabled, retryAt.Add(-time.Second), judgeOpts()); len(decisions) != 0 {
		t.Fatalf("early recovery produced %+v", decisions)
	}

	decisions := state.Advance(nil, disabled, retryAt, judgeOpts())
	if len(decisions) != 1 || decisions[0].Action != ActionEnable {
		t.Fatalf("decisions = %+v, want an enable", decisions)
	}
	slot := slotFor(t, state, cline1Key)
	if slot.DisabledByGuard || slot.Streak != 0 || slot.NextRetryAt != nil {
		t.Fatalf("slot after recovery = %+v, want cleared flags", slot)
	}
	if slot.DisableCount != 1 {
		t.Fatalf("DisableCount after recovery = %d, want 1 kept for escalation", slot.DisableCount)
	}
}

// TestRule9ManualRevert covers the first operator-intervention rule.
func TestRule9ManualRevert(t *testing.T) {
	state := NewState()
	state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
		smp(2, "r3", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false), witness()}, at(0), judgeOpts())

	// The operator enables the entry again, and a fresh burst arrives in the same
	// round: the guard yields and takes no action.
	decisions := state.Advance([]Sample{
		smp(3, "r4", cline1Key, "fireworks"),
		smp(4, "r5", cline1Key, "fireworks"),
		smp(5, "r6", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false), witness()}, at(10), judgeOpts())
	if len(decisions) != 0 {
		t.Fatalf("manual revert still acted: %+v", decisions)
	}
	slot := slotFor(t, state, cline1Key)
	if slot.DisabledByGuard || slot.Streak != 0 || slot.DisableCount != 0 || slot.NextRetryAt != nil {
		t.Fatalf("manual revert did not reset state: %+v", slot)
	}
}

// TestRule9ExternalDisable covers the second operator-intervention rule.
func TestRule9ExternalDisable(t *testing.T) {
	state := NewState()
	state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false), witness()}, at(2), judgeOpts())
	if got := slotFor(t, state, cline1Key).Streak; got != 2 {
		t.Fatalf("streak = %d, want 2", got)
	}

	decisions := state.Advance([]Sample{
		smp(2, "r3", cline1Key, "fireworks"),
		smp(3, "r4", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, true), witness()}, at(4), judgeOpts())
	if len(decisions) != 0 {
		t.Fatalf("externally disabled account produced %+v", decisions)
	}
	if got := slotFor(t, state, cline1Key).Streak; got != 0 {
		t.Fatalf("streak after external disable = %d, want 0", got)
	}
}

// TestRule10MasterOffStillRecovers covers rule 10.
func TestRule10MasterOffStillRecovers(t *testing.T) {
	state := NewState()
	state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
		smp(2, "r3", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false), witness()}, at(0), judgeOpts())
	retryAt := *slotFor(t, state, cline1Key).NextRetryAt

	off := judgeOpts()
	off.Enabled = false
	disabled := []Account{acct("Cline1", cline1Key, true), witness()}
	decisions := state.Advance([]Sample{
		smp(3, "r4", cline1Key, "fireworks"),
		smp(4, "r5", cline1Key, "fireworks"),
	}, disabled, retryAt.Add(-time.Minute), off)
	if len(decisions) != 0 {
		t.Fatalf("master off judged anyway: %+v", decisions)
	}

	decisions = state.Advance(nil, disabled, retryAt, off)
	if len(decisions) != 1 || decisions[0].Action != ActionEnable {
		t.Fatalf("master off skipped recovery: %+v", decisions)
	}
}

// TestRule11EmptyBaselineNoJudgement covers rule 11.
func TestRule11EmptyBaselineNoJudgement(t *testing.T) {
	opts := judgeOpts()
	opts.Baseline = ""
	state := NewState()
	decisions := state.Advance([]Sample{
		smp(0, "r1", cline1Key, "fireworks"),
		smp(1, "r2", cline1Key, "fireworks"),
		smp(2, "r3", cline1Key, "fireworks"),
	}, []Account{acct("Cline1", cline1Key, false), witness()}, at(3), opts)
	if len(decisions) != 0 {
		t.Fatalf("empty baseline judged: %+v", decisions)
	}
	if slot, ok := state.Accounts[cline1Key]; ok && slot.Streak != 0 {
		t.Fatalf("empty baseline counted a streak: %+v", slot)
	}

	// Recovery still runs with an empty baseline.
	state.Advance([]Sample{
		smp(3, "s1", cline2Key, "fireworks"),
		smp(4, "s2", cline2Key, "fireworks"),
		smp(5, "s3", cline2Key, "fireworks"),
	}, []Account{acct("Cline2", cline2Key, false), acct("Cline3", cline3Key, false)}, at(5), judgeOpts())
	retryAt := *slotFor(t, state, cline2Key).NextRetryAt
	decisions = state.Advance(nil, []Account{acct("Cline2", cline2Key, true)}, retryAt, opts)
	if len(decisions) != 1 || decisions[0].Action != ActionEnable {
		t.Fatalf("empty baseline skipped recovery: %+v", decisions)
	}
}

// TestRule12ScopeNames covers rule 12: only in-scope accounts are decided, out-of-scope
// accounts are still counted, and MinEnabled counts only in-scope accounts.
func TestRule12ScopeNames(t *testing.T) {
	accounts := []Account{
		acct("Cline1", cline1Key, false),
		acct("Cline2", cline2Key, false),
		acct("Cline3", cline3Key, false),
	}
	samples := []Sample{
		smp(0, "a1", cline1Key, "fireworks"),
		smp(1, "a2", cline1Key, "fireworks"),
		smp(2, "a3", cline1Key, "fireworks"),
		smp(3, "b1", cline2Key, "fireworks"),
		smp(4, "b2", cline2Key, "fireworks"),
		smp(5, "b3", cline2Key, "fireworks"),
		smp(6, "c1", cline3Key, "fireworks"),
		smp(7, "c2", cline3Key, "fireworks"),
		smp(8, "c3", cline3Key, "fireworks"),
	}

	// Scope of one account (matched case insensitively): MinEnabled's budget holds only
	// that account, so nothing is disabled, but every account is still counted.
	scoped := judgeOpts()
	scoped.ScopeNames = []string{"CLINE1"}
	state := NewState()
	decisions := state.Advance(samples, accounts, at(9), scoped)
	if len(decisions) != 0 {
		t.Fatalf("out-of-scope accounts shared the MinEnabled budget: %+v", decisions)
	}
	for _, key := range []string{cline1Key, cline2Key, cline3Key} {
		if slot := slotFor(t, state, key); slot.Streak != 3 || slot.DisabledByGuard {
			t.Fatalf("%s slot = %+v, want a counted but untouched streak of 3", key, slot)
		}
	}

	// Every account in scope: the floor allows two of the three.
	all := judgeOpts()
	all.ScopeNames = []string{"cline1", "cline2", "cline3"}
	state = NewState()
	decisions = state.Advance(samples, accounts, at(9), all)
	if len(decisions) != 2 || decisions[0].Name != "Cline1" || decisions[1].Name != "Cline2" {
		t.Fatalf("in-scope decisions = %+v, want Cline1 and Cline2", decisions)
	}
	if slot := slotFor(t, state, cline3Key); slot.Streak != 3 || slot.DisabledByGuard {
		t.Fatalf("blocked in-scope account = %+v, want a surviving streak", slot)
	}
}

// TestRule13Channels covers rule 13: ascending, capped at five, and limited to the
// streak that triggered the decision.
func TestRule13Channels(t *testing.T) {
	accounts := []Account{acct("Cline1", cline1Key, false), witness()}
	state := NewState()
	var samples []Sample
	for i := 0; i < 7; i++ {
		samples = append(samples, smp(i, "ch"+strconv.Itoa(i), cline1Key, "fw"+strconv.Itoa(i)))
	}
	decisions := state.Advance(samples, accounts, at(10), judgeOpts())
	if len(decisions) != 1 {
		t.Fatalf("decisions = %+v, want one", decisions)
	}
	want := []string{"fw2", "fw3", "fw4", "fw5", "fw6"}
	if len(decisions[0].Channels) != len(want) {
		t.Fatalf("channels = %v, want %v", decisions[0].Channels, want)
	}
	for i := range want {
		if decisions[0].Channels[i] != want[i] {
			t.Fatalf("channels = %v, want %v", decisions[0].Channels, want)
		}
	}

	// A baseline clears the memory, so the next decision lists only its own streak.
	state = NewState()
	state.Advance([]Sample{
		smp(0, "x1", cline1Key, "old1"),
		smp(1, "x2", cline1Key, "old2"),
	}, accounts, at(2), judgeOpts())
	decisions = state.Advance([]Sample{
		smp(2, "x3", cline1Key, "deepseek"),
		smp(3, "x4", cline1Key, "new1"),
		smp(4, "x5", cline1Key, "new2"),
		smp(5, "x6", cline1Key, "new3"),
	}, accounts, at(6), judgeOpts())
	if len(decisions) != 1 {
		t.Fatalf("decisions = %+v, want one", decisions)
	}
	if got := decisions[0].Channels; len(got) != 3 || got[0] != "new1" || got[2] != "new3" {
		t.Fatalf("channels after a reset = %v, want [new1 new2 new3]", got)
	}
}

// TestBackoffDoublingAndCap covers the exponential backoff and its ceiling across
// several disable/recover cycles.
func TestBackoffDoublingAndCap(t *testing.T) {
	state := NewState()
	opts := judgeOpts()
	want := []time.Duration{
		30 * time.Minute,
		60 * time.Minute,
		120 * time.Minute,
		240 * time.Minute,
		360 * time.Minute, // capped from 480
		360 * time.Minute, // capped from 960
	}
	now := guardBase
	sec := 0
	for cycle, wantBackoff := range want {
		samples := []Sample{
			smp(sec, "d"+strconv.Itoa(cycle)+"-1", cline1Key, "fireworks"),
			smp(sec+1, "d"+strconv.Itoa(cycle)+"-2", cline1Key, "fireworks"),
			smp(sec+2, "d"+strconv.Itoa(cycle)+"-3", cline1Key, "fireworks"),
		}
		sec += 3
		decisions := state.Advance(samples, []Account{acct("Cline1", cline1Key, false), witness()}, now, opts)
		if len(decisions) != 1 || decisions[0].Action != ActionDisable {
			t.Fatalf("cycle %d: decisions = %+v, want a disable", cycle, decisions)
		}
		slot := slotFor(t, state, cline1Key)
		if slot.DisableCount != cycle+1 {
			t.Fatalf("cycle %d: DisableCount = %d, want %d", cycle, slot.DisableCount, cycle+1)
		}
		if slot.NextRetryAt == nil || !slot.NextRetryAt.Equal(now.Add(wantBackoff)) {
			t.Fatalf("cycle %d: NextRetryAt = %v, want %v", cycle, slot.NextRetryAt, now.Add(wantBackoff))
		}
		now = *slot.NextRetryAt
		decisions = state.Advance(nil, []Account{acct("Cline1", cline1Key, true), witness()}, now, opts)
		if len(decisions) != 1 || decisions[0].Action != ActionEnable {
			t.Fatalf("cycle %d: decisions = %+v, want an enable", cycle, decisions)
		}
	}
}

// TestDefaults covers the zero and negative fallbacks, including the deliberate
// ReenableMinutes == 0 meaning "never".
func TestDefaults(t *testing.T) {
	t.Run("threshold and min enabled", func(t *testing.T) {
		state := NewState()
		opts := Options{Enabled: true, Baseline: "deepseek", Threshold: 0, MinEnabled: -7, ReenableMinutes: -1}
		decisions := state.Advance([]Sample{
			smp(0, "r1", cline1Key, "fireworks"),
			smp(1, "r2", cline1Key, "fireworks"),
			smp(2, "r3", cline1Key, "fireworks"),
		}, []Account{acct("Cline1", cline1Key, false), witness()}, at(3), opts)
		if len(decisions) != 1 || decisions[0].Streak != 3 {
			t.Fatalf("decisions = %+v, want one at the default threshold 3", decisions)
		}
		if got := *slotFor(t, state, cline1Key).NextRetryAt; !got.Equal(at(3).Add(30 * time.Minute)) {
			t.Fatalf("NextRetryAt = %v, want the default 30 minute step", got)
		}
	})

	t.Run("max disable default caps backoff", func(t *testing.T) {
		state := NewState()
		opts := Options{Enabled: true, Baseline: "deepseek", ReenableMinutes: 30, MaxDisableMinutes: -1}
		now := guardBase
		sec := 0
		accounts := []Account{acct("Cline1", cline1Key, false), witness()}
		for cycle := 0; cycle < 5; cycle++ {
			samples := []Sample{
				smp(sec, "c"+strconv.Itoa(cycle)+"-1", cline1Key, "fireworks"),
				smp(sec+1, "c"+strconv.Itoa(cycle)+"-2", cline1Key, "fireworks"),
				smp(sec+2, "c"+strconv.Itoa(cycle)+"-3", cline1Key, "fireworks"),
			}
			sec += 3
			state.Advance(samples, accounts, now, opts)
			now = *slotFor(t, state, cline1Key).NextRetryAt
			state.Advance(nil, []Account{acct("Cline1", cline1Key, true), witness()}, now, opts)
		}
		// Sixth cycle: 30*2^5 = 960 minutes, capped at the default 360.
		state.Advance([]Sample{
			smp(sec, "c5-1", cline1Key, "fireworks"),
			smp(sec+1, "c5-2", cline1Key, "fireworks"),
			smp(sec+2, "c5-3", cline1Key, "fireworks"),
		}, accounts, now, opts)
		if got := *slotFor(t, state, cline1Key).NextRetryAt; !got.Equal(now.Add(360 * time.Minute)) {
			t.Fatalf("NextRetryAt = %v, want the 360 minute cap", got)
		}
	})

	t.Run("reenable zero never recovers", func(t *testing.T) {
		opts := judgeOpts()
		opts.ReenableMinutes = 0
		state := NewState()
		state.Advance([]Sample{
			smp(0, "r1", cline1Key, "fireworks"),
			smp(1, "r2", cline1Key, "fireworks"),
			smp(2, "r3", cline1Key, "fireworks"),
		}, []Account{acct("Cline1", cline1Key, false), witness()}, at(0), opts)
		if slotFor(t, state, cline1Key).NextRetryAt != nil {
			t.Fatalf("NextRetryAt set with ReenableMinutes 0")
		}
		decisions := state.Advance(nil, []Account{acct("Cline1", cline1Key, true), witness()}, at(0).Add(100*time.Hour), opts)
		if len(decisions) != 0 {
			t.Fatalf("never-reenable account recovered: %+v", decisions)
		}
	})
}
