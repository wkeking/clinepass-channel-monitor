package hostconf

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/wkeking/clinepass-channel-monitor/internal/config"
)

// This file is the second half of the package: the openai-compatibility account list, and the
// one-scalar writer the account guard uses to flip an entry's `disabled`.
//
// Two rules keep the writer honest, because it edits a file the host owns. It touches one scalar
// and nothing else: the output is the input bytes with exactly one token replaced (or one line
// inserted), so comments, ordering and the operator's formatting survive, which re-marshalling
// the document would not do. And it refuses instead of guessing: an entry that has no `disabled`
// key and lives in flow style is reported as ErrFlowStyle, so the operator adds that key once
// and every later flip is a plain token replacement.

// ErrEntryNotFound reports that no openai-compatibility entry carries the name asked for.
var ErrEntryNotFound = errors.New("openai-compatibility entry not found")

// ErrFlowStyle reports that the entry has no `disabled` key and is written in flow style, where a
// scalar cannot be inserted without rewriting the line.
var ErrFlowStyle = errors.New("openai-compatibility entry has no disabled key and is in flow style")

// providerPrefix is CPA's internal key prefix for an openai-compatibility entry, used verbatim so
// the provider name this plugin sees on a record matches the one CPA synthesizes
// (internal/util/provider.go: OpenAICompatibleProviderKey).
const providerPrefix = "openai-compatible-"

// Entry is one openai-compatibility account as CPA's configuration file describes it. It carries
// no credential value: a name, a boolean and model aliases only.
type Entry struct {
	// Index is the position in the entry list, which is what the management API accepts too.
	Index int `json:"index"`
	// Name is the entry name, e.g. "Cline1".
	Name string `json:"name"`
	// BaseURL is the entry's upstream base URL. It carries no credential, and it is what tells
	// a Cline account apart from the other openai-compatibility entries in the same list
	// (config.Config.IsClineEntry).
	BaseURL string `json:"base_url,omitempty"`
	// Disabled is the entry's own switch; a missing key means enabled.
	Disabled bool `json:"disabled"`
	// ProviderKey is the name a record reports in cpa_provider, e.g. "openai-compatible-cline1".
	ProviderKey string `json:"provider_key"`
	// Aliases are the model aliases the entry serves (models[].alias), empty ones dropped.
	Aliases []string `json:"aliases,omitempty"`
}

// ProviderKey applies CPA's naming rule to an entry name: the name is trimmed and lowercased, an
// empty name or the literal "openai-compatibility" stays as is, and a name that already carries
// the prefix is not prefixed twice.
func ProviderKey(name string) string {
	trimmed := strings.ToLower(strings.TrimSpace(name))
	if trimmed == "" {
		return "openai-compatibility"
	}
	if trimmed == "openai-compatibility" || strings.HasPrefix(trimmed, providerPrefix) {
		return trimmed
	}
	return providerPrefix + trimmed
}

// ReadRaw returns the first CPA configuration file that can be read, with its bytes. The path is
// reported beside the bytes so a caller can log where a change would land.
func ReadRaw(cfg config.Config) (string, []byte, error) {
	paths := Paths(cfg.PlanConfigPath)
	var lastErr error
	for _, path := range paths {
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			lastErr = errRead
			continue
		}
		return path, raw, nil
	}
	if lastErr == nil {
		lastErr = os.ErrNotExist
	}
	return paths[len(paths)-1], nil, lastErr
}

// Entries lists the openai-compatibility accounts in either layout CPA has used: the classic
// top-level `openai-compatibility` list, and the v8 `api-keys.openai-compatibility` list. A file
// that cannot be parsed, or one without such a list, yields nil.
func Entries(raw []byte) []Entry {
	var document yaml.Node
	if errUnmarshal := yaml.Unmarshal(raw, &document); errUnmarshal != nil {
		return nil
	}
	list := openAICompatList(documentRoot(&document))
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil
	}
	entries := make([]Entry, 0, len(list.Content))
	for index, node := range list.Content {
		if node == nil || node.Kind != yaml.MappingNode {
			continue
		}
		entry := Entry{Index: index}
		entry.Name = scalarString(mapValue(node, "name"))
		entry.BaseURL = scalarString(mapValue(node, "base-url"))
		entry.Disabled = boolValue(mapValue(node, "disabled"))
		entry.ProviderKey = ProviderKey(entry.Name)
		entry.Aliases = entryAliases(node)
		entries = append(entries, entry)
	}
	return entries
}

// SetEntryDisabled flips one entry's `disabled` and returns the new file bytes. changed is false
// when the entry already carries the wanted value (the bytes come back untouched), and
// ErrEntryNotFound / ErrFlowStyle report the cases the caller has to look at itself. Everything
// outside that one token is preserved byte for byte.
func SetEntryDisabled(raw []byte, name string, disabled bool) ([]byte, bool, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return raw, false, ErrEntryNotFound
	}
	var document yaml.Node
	if errUnmarshal := yaml.Unmarshal(raw, &document); errUnmarshal != nil {
		return raw, false, fmt.Errorf("parse configuration: %w", errUnmarshal)
	}
	list := openAICompatList(documentRoot(&document))
	if list == nil || list.Kind != yaml.SequenceNode {
		return raw, false, ErrEntryNotFound
	}
	target := findEntry(list, trimmed)
	if target == nil {
		return raw, false, ErrEntryNotFound
	}
	literal := "false"
	if disabled {
		literal = "true"
	}
	if value := mapValue(target, "disabled"); value != nil {
		if boolValue(value) == disabled {
			return raw, false, nil
		}
		out, errReplace := replaceScalar(raw, value, literal)
		if errReplace != nil {
			return raw, false, errReplace
		}
		return out, true, nil
	}
	if target.Style == yaml.FlowStyle {
		return raw, false, ErrFlowStyle
	}
	out, errInsert := insertMappingKey(raw, target, "disabled", literal)
	if errInsert != nil {
		return raw, false, errInsert
	}
	return out, true, nil
}

// openAICompatList returns the sequence node that holds the openai-compatibility entries, in
// either layout, or nil when the file has none.
func openAICompatList(root *yaml.Node) *yaml.Node {
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	if apiKeys := mapValue(root, "api-keys"); apiKeys != nil && apiKeys.Kind == yaml.MappingNode {
		if list := mapValue(apiKeys, "openai-compatibility"); list != nil && list.Kind == yaml.SequenceNode {
			return list
		}
	}
	if list := mapValue(root, "openai-compatibility"); list != nil && list.Kind == yaml.SequenceNode {
		return list
	}
	return nil
}

// findEntry returns the mapping node of the entry whose `name` equals name.
func findEntry(list *yaml.Node, name string) *yaml.Node {
	for _, node := range list.Content {
		if node == nil || node.Kind != yaml.MappingNode {
			continue
		}
		if strings.TrimSpace(scalarString(mapValue(node, "name"))) == name {
			return node
		}
	}
	return nil
}

// entryAliases collects models[].alias, skipping the empty ones.
func entryAliases(entry *yaml.Node) []string {
	models := mapValue(entry, "models")
	if models == nil || models.Kind != yaml.SequenceNode {
		return nil
	}
	var aliases []string
	for _, model := range models.Content {
		if model == nil || model.Kind != yaml.MappingNode {
			continue
		}
		if alias := strings.TrimSpace(scalarString(mapValue(model, "alias"))); alias != "" {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

// boolValue decodes a scalar node as a bool. A missing or non-boolean node is false, which is what
// CPA means by an absent `disabled`.
func boolValue(node *yaml.Node) bool {
	pointer := boolPointer(node)
	return pointer != nil && *pointer
}

// replaceScalar rewrites the source token of one node in place, leaving every other byte alone.
func replaceScalar(raw []byte, node *yaml.Node, literal string) ([]byte, error) {
	if node.Line <= 0 || node.Column <= 0 {
		return nil, fmt.Errorf("yaml node has no source position")
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	if node.Line > len(lines) {
		return nil, fmt.Errorf("yaml node line %d is outside the file", node.Line)
	}
	line := lines[node.Line-1]
	start := columnOffset(line, node.Column)
	if start < 0 || start > len(line) {
		return nil, fmt.Errorf("yaml node column %d is outside line %d", node.Column, node.Line)
	}
	end := tokenEnd(line, start)
	updated := make([]byte, 0, len(line)+len(literal))
	updated = append(updated, line[:start]...)
	updated = append(updated, literal...)
	updated = append(updated, line[end:]...)
	lines[node.Line-1] = updated
	return bytes.Join(lines, nil), nil
}

// insertMappingKey adds one `key: literal` line as the last key of a block mapping. The mapping's
// own indentation is taken from its first key, and the insertion point is the last line any
// descendant of the mapping sits on, which leaves nested keys/models blocks intact.
func insertMappingKey(raw []byte, mapping *yaml.Node, key, literal string) ([]byte, error) {
	if len(mapping.Content) < 2 || mapping.Content[0].Line <= 0 {
		return nil, fmt.Errorf("yaml mapping has no source position")
	}
	anchor := mapping.Content[0]
	last := maxLine(mapping)
	if last <= 0 {
		return nil, fmt.Errorf("yaml mapping has no source position")
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	if last > len(lines) {
		return nil, fmt.Errorf("yaml mapping line %d is outside the file", last)
	}
	inserted := make([]byte, 0, anchor.Column+len(key)+len(literal)+4)
	for index := 1; index < anchor.Column; index++ {
		inserted = append(inserted, ' ')
	}
	inserted = append(inserted, key...)
	inserted = append(inserted, ':', ' ')
	inserted = append(inserted, literal...)
	inserted = append(inserted, '\n')

	out := make([]byte, 0, len(raw)+len(inserted))
	for index, line := range lines {
		out = append(out, line...)
		if index == last-1 {
			if len(line) == 0 || line[len(line)-1] != '\n' {
				out = append(out, '\n')
			}
			out = append(out, inserted...)
		}
	}
	return out, nil
}

// maxLine returns the greatest 1-based source line any node of the subtree sits on.
func maxLine(node *yaml.Node) int {
	if node == nil {
		return 0
	}
	greatest := node.Line
	for _, child := range node.Content {
		if childLine := maxLine(child); childLine > greatest {
			greatest = childLine
		}
	}
	return greatest
}

// columnOffset converts a 1-based column into a byte offset in the line. yaml.v3 counts columns
// in characters, so the line is decoded rune by rune; non-ASCII comments above an entry are
// common in this file.
func columnOffset(line []byte, column int) int {
	offset := 0
	for index := 1; index < column; index++ {
		if offset >= len(line) {
			return -1
		}
		_, size := utf8.DecodeRune(line[offset:])
		if size <= 0 {
			return -1
		}
		offset += size
	}
	return offset
}

// tokenEnd returns the byte offset just past the scalar token that starts at start: after the
// closing quote for a quoted scalar, otherwise up to the first delimiter that can follow a plain
// scalar (whitespace, a block/flow separator, or a comment).
func tokenEnd(line []byte, start int) int {
	if start >= len(line) {
		return len(line)
	}
	switch line[start] {
	case '"', '\'':
		quote := line[start]
		index := start + 1
		for index < len(line) {
			if line[index] == quote {
				if quote == '\'' && index+1 < len(line) && line[index+1] == '\'' {
					index += 2
					continue
				}
				return index + 1
			}
			if quote == '"' && line[index] == '\\' {
				index += 2
				continue
			}
			index++
		}
		return len(line)
	}
	for index := start; index < len(line); index++ {
		switch line[index] {
		case ' ', '\t', '\r', '\n', ',', '}', ']', '#':
			return index
		}
	}
	return len(line)
}
