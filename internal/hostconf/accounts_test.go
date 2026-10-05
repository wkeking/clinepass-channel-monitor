package hostconf

import (
	"errors"
	"strings"
	"testing"
)

// v8BlockFixture mirrors the shape CPA leaves behind once it has written the file itself: the v8
// layout (api-keys.openai-compatibility) in block style with quoted keys, comments and a plugin
// block that must survive every edit. Both fixtures use sk-FIXTURE… values so the secret scan
// recognises them as synthetic.
const v8BlockFixture = `# 顶部注释：改动不能碰这里
plugins:
    enabled: true
    configs:
        clinepass-channel-monitor:
            channel_log_enabled: true   # 守卫要保护的就是这一行
            enabled: true
observability:
    logs:
        request-log: true
api-keys:
    codex: []
    claude: []
    openai-compatibility:
        - "base-url": "https://api.deepseek.com"
          "disabled": true
          "keys":
            - "api-key": "sk-FIXTURE-deepseek"
          "models":
            - "alias": "deepseek-flash"
              "name": "deepseek-flash"
          "name": "DeepSeek"
        - "base-url": "https://api.cline.bot/api/v1"
          "disabled": false
          "keys":
            - "api-key": "sk-FIXTURE-cline1"
          "models":
            - "alias": "deepseek-flash-1"
              "name": "cline-pass/deepseek-v4.1-flash"
          "name": "Cline1"
`

// v7BlockFixture is the classic layout in block style, and its second entry has no `disabled` key
// at all, which is the insert path.
const v7BlockFixture = `# 顶部注释
openai-compatibility:
    - name: DeepSeek
      base-url: https://api.deepseek.com
      disabled: false
      api-key-entries:
        - api-key: sk-FIXTURE-deepseek
    - name: Cline1
      base-url: https://api.cline.bot/api/v1
      api-key-entries:
        - api-key: sk-FIXTURE-cline1
`

// v7FlowFixture is what the file looked like before CPA ever wrote it: one flow-style line.
const v7FlowFixture = `openai-compatibility: [{"base-url": "https://api.deepseek.com", "disabled": true, "name": "DeepSeek"}, {"base-url": "https://api.cline.bot/api/v1", "disabled": false, "name": "Cline1"}]
`

const v7FlowNoDisabledFixture = `openai-compatibility: [{"base-url": "https://api.deepseek.com", "name": "DeepSeek"}]
`

func TestProviderKeyFollowsCPAsNaming(t *testing.T) {
	for name, want := range map[string]string{
		"Cline1":                   "openai-compatible-cline1",
		"  Cline1  ":               "openai-compatible-cline1",
		"UGQ DS":                   "openai-compatible-ugq ds",
		"openai-compatible-cline1": "openai-compatible-cline1",
		"openai-compatibility":     "openai-compatibility",
		"":                         "openai-compatibility",
		"openai-compatible-":       "openai-compatible-",
		"DeepSeek":                 "openai-compatible-deepseek",
		"openai-compatible-cLInE1": "openai-compatible-cline1",
		// CPA only recognises the "openai-compatible-" prefix, so a name that merely starts with
		// "openai-compatibility" gets the prefix glued on — reproduced on purpose.
		"openai-compatibility-cline9":  "openai-compatible-openai-compatibility-cline9",
		"openai-compatible-providerXX": "openai-compatible-providerxx",
	} {
		if got := ProviderKey(name); got != want {
			t.Errorf("ProviderKey(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestEntriesReadsTheV8Layout(t *testing.T) {
	entries := Entries([]byte(v8BlockFixture))
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %#v", len(entries), entries)
	}
	first := entries[0]
	if first.Index != 0 || first.Name != "DeepSeek" || !first.Disabled {
		t.Errorf("entry 0 = %#v", first)
	}
	if first.ProviderKey != "openai-compatible-deepseek" {
		t.Errorf("entry 0 provider key = %q", first.ProviderKey)
	}
	if len(first.Aliases) != 1 || first.Aliases[0] != "deepseek-flash" {
		t.Errorf("entry 0 aliases = %#v", first.Aliases)
	}
	second := entries[1]
	if second.Index != 1 || second.Name != "Cline1" || second.Disabled {
		t.Errorf("entry 1 = %#v", second)
	}
	if second.ProviderKey != "openai-compatible-cline1" {
		t.Errorf("entry 1 provider key = %q", second.ProviderKey)
	}
	if len(second.Aliases) != 1 || second.Aliases[0] != "deepseek-flash-1" {
		t.Errorf("entry 1 aliases = %#v", second.Aliases)
	}
}

func TestEntriesReadsTheClassicLayoutAndMissingDisabled(t *testing.T) {
	entries := Entries([]byte(v7BlockFixture))
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %#v", len(entries), entries)
	}
	if entries[0].Name != "DeepSeek" || entries[0].Disabled {
		t.Errorf("entry 0 = %#v, want enabled DeepSeek", entries[0])
	}
	if entries[1].Name != "Cline1" || entries[1].Disabled {
		t.Errorf("entry 1 = %#v, want a missing disabled key read as enabled", entries[1])
	}
	if len(entries[1].Aliases) != 0 {
		t.Errorf("entry 1 aliases = %#v, want none (no models block)", entries[1].Aliases)
	}
}

func TestEntriesToleratesGarbage(t *testing.T) {
	if entries := Entries([]byte("\tnot: [yaml")); entries != nil {
		t.Errorf("Entries on broken YAML = %#v, want nil", entries)
	}
	if entries := Entries(nil); entries != nil {
		t.Errorf("Entries on empty input = %#v, want nil", entries)
	}
}

func TestSetEntryDisabledRewritesOnlyThatToken(t *testing.T) {
	out, changed, err := SetEntryDisabled([]byte(v8BlockFixture), "Cline1", true)
	if err != nil || !changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	want := strings.Replace(v8BlockFixture, `"disabled": false`, `"disabled": true`, 1)
	if string(out) != want {
		t.Errorf("output differs from the one-token rewrite:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
	// The plugin block, the top comment and the other entry must be untouched, byte for byte.
	for _, needle := range []string{
		"# 顶部注释：改动不能碰这里",
		"            channel_log_enabled: true   # 守卫要保护的就是这一行",
		`        - "base-url": "https://api.deepseek.com"`,
		`            - "api-key": "sk-FIXTURE-deepseek"`,
		`          "name": "DeepSeek"`,
		`          "name": "Cline1"`,
	} {
		if !strings.Contains(string(out), needle) {
			t.Errorf("output lost %q:\n%s", needle, out)
		}
	}
}

func TestSetEntryDisabledChangesExactlyOneLine(t *testing.T) {
	out, changed, err := SetEntryDisabled([]byte(v8BlockFixture), "Cline1", true)
	if err != nil || !changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	before := strings.Split(v8BlockFixture, "\n")
	after := strings.Split(string(out), "\n")
	if len(before) != len(after) {
		t.Fatalf("line count changed: %d → %d", len(before), len(after))
	}
	differing := 0
	for index := range before {
		if before[index] != after[index] {
			differing++
			if !strings.Contains(before[index], `"disabled": false`) || !strings.Contains(after[index], `"disabled": true`) {
				t.Errorf("line %d changed in an unexpected way:\n  before: %q\n  after : %q", index+1, before[index], after[index])
			}
		}
	}
	if differing != 1 {
		t.Errorf("%d lines differ, want exactly 1", differing)
	}
}

func TestSetEntryDisabledIsIdempotent(t *testing.T) {
	out, changed, err := SetEntryDisabled([]byte(v8BlockFixture), "DeepSeek", true)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if changed {
		t.Error("changed = true for a value that is already set")
	}
	if string(out) != v8BlockFixture {
		t.Error("bytes changed even though nothing had to be written")
	}
}

func TestSetEntryDisabledInsertsAMissingKey(t *testing.T) {
	out, changed, err := SetEntryDisabled([]byte(v7BlockFixture), "Cline1", true)
	if err != nil || !changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	if want := v7BlockFixture + "      disabled: true\n"; string(out) != want {
		t.Errorf("inserted line differs:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
	entries := Entries(out)
	if len(entries) != 2 || !entries[1].Disabled || entries[0].Disabled == entries[1].Disabled {
		t.Errorf("re-read entries = %#v, want only Cline1 disabled", entries)
	}
}

func TestSetEntryDisabledSplicesAFlowStyleValue(t *testing.T) {
	out, changed, err := SetEntryDisabled([]byte(v7FlowFixture), "DeepSeek", false)
	if err != nil || !changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	want := strings.Replace(v7FlowFixture, `"disabled": true`, `"disabled": false`, 1)
	if string(out) != want {
		t.Errorf("flow-style rewrite differs:\n got: %s\nwant: %s", out, want)
	}
	if entries := Entries(out); len(entries) != 2 || entries[0].Disabled {
		t.Errorf("re-read entries = %#v, want DeepSeek enabled", entries)
	}
}

func TestSetEntryDisabledRefusesToInsertInFlowStyle(t *testing.T) {
	out, changed, err := SetEntryDisabled([]byte(v7FlowNoDisabledFixture), "DeepSeek", true)
	if !errors.Is(err, ErrFlowStyle) {
		t.Fatalf("err = %v, want ErrFlowStyle", err)
	}
	if changed || string(out) != v7FlowNoDisabledFixture {
		t.Errorf("a refusal must not change the bytes (changed=%v)", changed)
	}
}

func TestSetEntryDisabledRejectsAnUnknownEntry(t *testing.T) {
	for _, fixture := range []string{v8BlockFixture, v7BlockFixture} {
		out, changed, err := SetEntryDisabled([]byte(fixture), "Cline9", true)
		if !errors.Is(err, ErrEntryNotFound) {
			t.Fatalf("err = %v, want ErrEntryNotFound", err)
		}
		if changed || string(out) != fixture {
			t.Error("a missing entry must leave the bytes alone")
		}
	}
}

func TestSetEntryDisabledRewritesAQuotedValue(t *testing.T) {
	fixture := strings.Replace(v8BlockFixture, `"disabled": false`, `"disabled": "false"`, 1)
	out, changed, err := SetEntryDisabled([]byte(fixture), "Cline1", true)
	if err != nil || !changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}
	if !strings.Contains(string(out), `"disabled": true,`) && !strings.Contains(string(out), `"disabled": true`+"\n") {
		t.Errorf("quoted value was not replaced by a plain bool:\n%s", out)
	}
	if entries := Entries(out); len(entries) != 2 || !entries[1].Disabled {
		t.Errorf("re-read entries = %#v, want Cline1 disabled", entries)
	}
}

func TestSetEntryDisabledRejectsBrokenYAML(t *testing.T) {
	out, changed, err := SetEntryDisabled([]byte("\tnot: [yaml"), "Cline1", true)
	if err == nil {
		t.Fatal("broken YAML must be an error, not a silent no-op")
	}
	if changed {
		t.Error("changed = true for broken YAML")
	}
	if string(out) != "\tnot: [yaml" {
		t.Error("broken input must be returned untouched")
	}
}
