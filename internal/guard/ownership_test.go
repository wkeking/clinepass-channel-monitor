package guard

import (
	"testing"
	"time"
)

// TestAdoptOwnershipTakesBackAnAccountTheAuditStillClaims covers the recovery a lost state file
// used to cost. The audit trail is append-only and shared by every instance, so it still says the
// guard disabled the account; the state file is rewritten every few seconds by every running
// instance, so the "the guard did this" flag can be gone. Without adoption the guard reads its own
// disable as somebody else's (rule 9b), forgets the streak and never brings the account back.
func TestAdoptOwnershipTakesBackAnAccountTheAuditStillClaims(t *testing.T) {
	state := NewState()
	accounts := []Account{acct("Cline1", cline1Key, true), witness()}
	audit := []Decision{{
		Time: at(0), Name: "Cline1", ProviderKey: cline1Key,
		Action: ActionDisable, Streak: 3, Reason: "Cline1 连续 3 次真实渠道不是基准渠道 deepseek，关闭该账号",
	}}

	taken := state.AdoptOwnership(accounts, audit, at(60), judgeOpts())
	if len(taken) != 1 || taken[0] != "Cline1" {
		t.Fatalf("adopted = %v, want [Cline1]", taken)
	}
	slot := slotFor(t, state, cline1Key)
	if !slot.DisabledByGuard {
		t.Error("disabled_by_guard = false, want the guard to own its own disable again")
	}
	if slot.DisableCount < 1 {
		t.Errorf("disable_count = %d, want at least 1 so the backoff does not restart from scratch", slot.DisableCount)
	}
	if slot.NextRetryAt == nil || !slot.NextRetryAt.Equal(at(60).Add(30*time.Minute)) {
		t.Errorf("next_retry_at = %v, want %v", slot.NextRetryAt, at(60).Add(30*time.Minute))
	}
}

// TestAdoptOwnershipLeavesOtherPeoplesDisablesAlone covers every way the audit trail says the
// account is not the guard's business: the last action was a dry-run report (nothing was written to
// the file), the last action was an enable (an operator may have disabled the account by hand
// afterwards), the audit has nothing to say about it, and the account is not disabled at all.
func TestAdoptOwnershipLeavesOtherPeoplesDisablesAlone(t *testing.T) {
	cases := []struct {
		name     string
		disabled bool
		action   string
		recorded bool
	}{
		{"a dry-run report wrote nothing", true, ActionDisableDry, true},
		{"the last action was an enable", true, ActionEnable, true},
		{"the audit has nothing to say", true, "", false},
		{"the account is not disabled", false, ActionDisable, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			state := NewState()
			accounts := []Account{acct("Cline1", cline1Key, testCase.disabled), witness()}
			var audit []Decision
			if testCase.recorded {
				audit = append(audit, Decision{
					Time: at(0), Name: "Cline1", ProviderKey: cline1Key,
					Action: testCase.action, Streak: 3,
				})
			}
			if taken := state.AdoptOwnership(accounts, audit, at(60), judgeOpts()); len(taken) != 0 {
				t.Fatalf("adopted = %v, want nothing", taken)
			}
			if slot, ok := state.Accounts[cline1Key]; ok && slot != nil && slot.DisabledByGuard {
				t.Error("the guard must not claim a disable the audit does not attribute to it")
			}
		})
	}
}

// TestAdoptOwnershipKeepsAnExistingSchedule pins idempotence: adoption runs on every plugin load,
// and an account the state file already owns must keep the schedule it was on - a fresh NextRetryAt
// each time would push the recovery out forever.
func TestAdoptOwnershipKeepsAnExistingSchedule(t *testing.T) {
	state := NewState()
	slot := state.account(cline1Key)
	slot.DisabledByGuard = true
	slot.DisableCount = 4
	retryAt := at(600)
	slot.NextRetryAt = &retryAt
	audit := []Decision{{
		Time: at(0), Name: "Cline1", ProviderKey: cline1Key, Action: ActionDisable, Streak: 3,
	}}

	taken := state.AdoptOwnership([]Account{acct("Cline1", cline1Key, true), witness()}, audit, at(60), judgeOpts())
	if len(taken) != 0 {
		t.Fatalf("adopted = %v, want nothing for an account the state file already owns", taken)
	}
	if slot.DisableCount != 4 || slot.NextRetryAt == nil || !slot.NextRetryAt.Equal(retryAt) {
		t.Errorf("disable_count = %d, next_retry_at = %v, want the existing schedule kept", slot.DisableCount, slot.NextRetryAt)
	}
}

// TestAdoptOwnershipThenRecovers pins the whole point of adopting: once the account is owned again,
// the ordinary state machine walks it back to enabled when the backoff expires instead of leaving it
// off because "somebody else" disabled it.
func TestAdoptOwnershipThenRecovers(t *testing.T) {
	opts := judgeOpts()
	state := NewState()
	accounts := []Account{acct("Cline1", cline1Key, true), witness()}
	audit := []Decision{{
		Time: at(0), Name: "Cline1", ProviderKey: cline1Key, Action: ActionDisable, Streak: 3,
	}}
	if taken := state.AdoptOwnership(accounts, audit, at(60), opts); len(taken) != 1 {
		t.Fatalf("adopted = %v, want [Cline1]", taken)
	}

	// Nothing is due yet: the backoff runs for 30 minutes from the adoption.
	if decisions := state.Advance(nil, accounts, at(120), opts); len(decisions) != 0 {
		t.Fatalf("decisions = %#v, want nothing before the backoff expires", decisions)
	}
	decisions := state.Advance(nil, accounts, at(60+30*60), opts)
	if len(decisions) != 1 || decisions[0].Action != ActionEnable {
		t.Fatalf("decisions = %#v, want one enable once the backoff expired", decisions)
	}
	if decisions[0].Name != "Cline1" {
		t.Errorf("enabled %q, want Cline1", decisions[0].Name)
	}
}
