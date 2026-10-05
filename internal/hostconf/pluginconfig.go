package hostconf

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file writes the plugin's OWN configuration block, and it exists because of how the host's
// configuration panel works.
//
// The panel renders whatever plugins.configs.<id> holds. It does not merge in the values the
// plugin would use when a key is absent, and the field schema a plugin declares
// (sdk/pluginapi.ConfigField) carries Name/Type/EnumValues/Description only - there is no
// default-value field for the panel to show. So an install that writes just `enabled` and
// `store` renders as a form full of empty boxes even though every one of those keys has a
// working default in the plugin.
//
// The only way to make the panel show real values is therefore to put them in the file, which is
// what this writer does - under the same two rules as the openai-compatibility writer:
//
//   - a key that is already present is never touched, whatever the operator set it to;
//   - everything outside the added lines survives byte for byte, comments included.
//
// It adds keys only. A key the operator deleted stays deleted until the next load adds it back
// with the value the plugin is actually using.

// ErrPluginConfigMissing reports that config.yaml has no plugins.configs.<id> mapping to fill.
var ErrPluginConfigMissing = errors.New("plugins.configs.<id> block not found")

// ErrPluginConfigNotEditable reports that the block has no source range this writer can insert
// into - an aliased or generated node rather than the operator's text.
var ErrPluginConfigNotEditable = errors.New("plugins.configs.<id> block cannot take an inserted key")

// PluginDefault is one key to add when the plugin's configuration block does not carry it.
type PluginDefault struct {
	// Key is the configuration key under plugins.configs.<id>.
	Key string
	// Literal is the value as a YAML token, already encoded (yaml.Marshal of the Go value), so
	// quoting and booleans are the encoder's decision rather than this writer's.
	Literal string
}

// PluginConfigDefaults adds every PluginDefault whose key is absent from the plugin's block and
// returns the new file bytes. changed is false when nothing had to be added, and the input bytes
// come back untouched.
//
// It is all or nothing: any error returns the original bytes, so a caller that writes the result
// can never persist a half-filled file.
func PluginConfigDefaults(raw []byte, pluginID string, defaults []PluginDefault) ([]byte, bool, error) {
	id := strings.TrimSpace(pluginID)
	if id == "" || len(defaults) == 0 {
		return raw, false, ErrPluginConfigMissing
	}
	if _, errBlock := pluginConfigBlock(raw, id); errBlock != nil {
		return raw, false, errBlock
	}
	out := raw
	changed := false
	for _, item := range defaults {
		key := strings.TrimSpace(item.Key)
		if key == "" || strings.TrimSpace(item.Literal) == "" {
			continue
		}
		// Re-parse each round: an inserted line moves every line number below it, and the
		// insertion point is derived from them.
		current, errCurrent := pluginConfigBlock(out, id)
		if errCurrent != nil {
			return raw, false, errCurrent
		}
		if mapValue(current, key) != nil {
			continue
		}
		// CPA writes this block in flow style when it saves the configuration itself (one
		// `{...}` line), and an operator who edits the file by hand writes block style. Both
		// shapes are common, so both get an insertion that touches nothing else.
		insert := insertMappingKey
		if current.Style == yaml.FlowStyle {
			insert = insertFlowMappingKey
		}
		updated, errInsert := insert(out, current, key, item.Literal)
		if errInsert != nil {
			return raw, false, fmt.Errorf("%w: %v", ErrPluginConfigNotEditable, errInsert)
		}
		out, changed = updated, true
	}
	if !changed {
		return raw, false, nil
	}
	return out, true, nil
}

// pluginConfigBlock returns the mapping node of plugins.configs.<id>, or an error saying why it
// cannot be edited.
func pluginConfigBlock(raw []byte, id string) (*yaml.Node, error) {
	var document yaml.Node
	if errUnmarshal := yaml.Unmarshal(raw, &document); errUnmarshal != nil {
		return nil, fmt.Errorf("parse configuration: %w", errUnmarshal)
	}
	root := documentRoot(&document)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, ErrPluginConfigMissing
	}
	plugins := mapValue(root, "plugins")
	if plugins == nil || plugins.Kind != yaml.MappingNode {
		return nil, ErrPluginConfigMissing
	}
	configs := mapValue(plugins, "configs")
	if configs == nil || configs.Kind != yaml.MappingNode {
		return nil, ErrPluginConfigMissing
	}
	block := mapValue(configs, id)
	if block == nil || block.Kind != yaml.MappingNode {
		return nil, ErrPluginConfigMissing
	}
	return block, nil
}

// insertFlowMappingKey adds `"key": literal` inside a `{...}` mapping, right before its closing
// brace, and leaves every other byte alone. Keys are quoted the way CPA quotes its own writes.
func insertFlowMappingKey(raw []byte, mapping *yaml.Node, key, literal string) ([]byte, error) {
	if mapping == nil || mapping.Line <= 0 || mapping.Column <= 0 {
		return nil, fmt.Errorf("yaml mapping has no source position")
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	if mapping.Line > len(lines) {
		return nil, fmt.Errorf("yaml mapping line %d is outside the file", mapping.Line)
	}
	start := 0
	for index := 0; index < mapping.Line-1; index++ {
		start += len(lines[index])
	}
	column := columnOffset(lines[mapping.Line-1], mapping.Column)
	if column < 0 {
		return nil, fmt.Errorf("yaml mapping column %d is outside line %d", mapping.Column, mapping.Line)
	}
	start += column
	if start >= len(raw) || raw[start] != '{' {
		return nil, fmt.Errorf("yaml mapping at line %d does not start with '{'", mapping.Line)
	}
	end := flowMappingEnd(raw, start)
	if end < 0 {
		return nil, fmt.Errorf("flow mapping at line %d has no closing brace", mapping.Line)
	}
	entry := fmt.Sprintf("%q: %s", key, literal)
	inserted := entry
	if first := skipFlowSpace(raw, start+1, end); first < end {
		// Non-empty mapping: keep the existing separator style, adding a comma only when the
		// mapping does not already end with one.
		last := end - 1
		for last > start && isFlowSpace(raw[last]) {
			last--
		}
		if raw[last] == ',' {
			inserted = " " + entry
		} else {
			inserted = ", " + entry
		}
	}
	out := make([]byte, 0, len(raw)+len(inserted))
	out = append(out, raw[:end]...)
	out = append(out, inserted...)
	out = append(out, raw[end:]...)
	return out, nil
}

// flowMappingEnd returns the index of the `}` that closes the flow mapping opening at start, or
// -1. It tracks nesting and skips quoted scalars, so a `}` inside a nested mapping or a string
// does not end the mapping early.
func flowMappingEnd(raw []byte, start int) int {
	depth := 0
	for index := start; index < len(raw); index++ {
		switch raw[index] {
		case '"', '\'':
			index = skipQuotedScalar(raw, index)
		case '{', '[':
			depth++
		case '}':
			depth--
			if depth <= 0 {
				return index
			}
		case ']':
			depth--
		}
	}
	return -1
}

// skipQuotedScalar returns the index of the quote that closes the scalar opened at start.
func skipQuotedScalar(raw []byte, start int) int {
	quote := raw[start]
	index := start + 1
	for index < len(raw) {
		if quote == '"' && raw[index] == '\\' {
			index += 2
			continue
		}
		if raw[index] == quote {
			if quote == '\'' && index+1 < len(raw) && raw[index+1] == '\'' {
				index += 2
				continue
			}
			return index
		}
		index++
	}
	return len(raw) - 1
}

// skipFlowSpace returns the offset of the first byte in [from, to) that is not flow whitespace.
func skipFlowSpace(raw []byte, from, to int) int {
	for index := from; index < to; index++ {
		if !isFlowSpace(raw[index]) {
			return index
		}
	}
	return to
}

func isFlowSpace(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n':
		return true
	}
	return false
}
