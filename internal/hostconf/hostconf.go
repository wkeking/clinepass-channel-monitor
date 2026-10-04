// Package hostconf reads CPA's own configuration file and reports a few key host
// settings for the management page's configuration self-check card.
//
// The read is deliberately narrow. The snapshot carries booleans, a number, a
// directory name, the key names of this plugin's block and two store fields, so it
// never contains a credential value: CPA persists Cline API keys and gateway keys in
// the same file (openai-compatibility, api-keys), and none of them may leave the host.
package hostconf

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
	"github.com/wkeking/clinepass-channel-monitor/internal/config"
)

// candidatePaths are the locations CPA's configuration can be read from inside the
// container and from a host-side deployment, in the same order the plan credential
// discovery uses.
var candidatePaths = []string{
	"/CLIProxyAPI/config.yaml",
	"/app/config.yaml",
}

// Paths returns the candidate paths of CPA's configuration file. A non-empty override
// is used alone; otherwise the two built-in candidates are returned in discovery order.
func Paths(override string) []string {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return []string{trimmed}
	}
	out := make([]string, len(candidatePaths))
	copy(out, candidatePaths)
	return out
}

// Snapshot is a read-only snapshot of a few host settings. It holds booleans, a number,
// a directory name, key names and two store fields, and never a credential value.
type Snapshot struct {
	Path               string   `json:"path"`
	Readable           bool     `json:"readable"`
	Error              string   `json:"error,omitempty"`
	PluginsEnabled     *bool    `json:"plugins_enabled,omitempty"`
	PluginsDir         string   `json:"plugins_dir,omitempty"`
	RequestLog         *bool    `json:"request_log,omitempty"`
	LogsMaxTotalSizeMB *int     `json:"logs_max_total_size_mb,omitempty"`
	CommercialMode     *bool    `json:"commercial_mode,omitempty"`
	PluginBlockKeys    []string `json:"plugin_block_keys,omitempty"`
	StoreVersion       string   `json:"store_version,omitempty"`
	StoreSource        string   `json:"store_source,omitempty"`
}

// GatewayHint describes why the gateway channel has no data yet, and which switches would fix
// it. It returns "" once the plugin-side switch is on, because that is the only half the plugin
// owns: the CPA-side half (request logging) is reported beside it. The caller logs this once per
// load, so a reloaded plugin says the same thing the page's self-check card will show.
func GatewayHint(cfg config.Config) string {
	if cfg.ChannelLogEnabled {
		return ""
	}
	hint := "网关渠道未启用：channel_log_enabled=false，「真实渠道」列与偏离指标会一直是空的"
	if snapshot := Read(cfg); snapshot.Readable {
		var missing []string
		// request-log 缺失时 CPA 的默认值就是 false，所以 nil 与 false 一样当作「没开」。
		if snapshot.RequestLog == nil || !*snapshot.RequestLog {
			missing = append(missing, "observability.logs.request-log: true")
		}
		if snapshot.CommercialMode != nil && *snapshot.CommercialMode {
			missing = append(missing, "server.commercial-mode: false")
		}
		if len(missing) > 0 {
			hint += "；CPA 侧还需要 " + strings.Join(missing, "、") + "，改完重启容器"
		}
	}
	return hint + "。管理页顶部的「配置自检」会按当前状态列出同样的清单"
}

// Read tries Paths(cfg.PlanConfigPath) in order and fills the snapshot from the first
// file it can read. A missing file is not an error in itself: when none of the
// candidates can be read the snapshot reports the last path and the last error.
func Read(cfg config.Config) Snapshot {
	paths := Paths(cfg.PlanConfigPath)
	var lastErr error
	for _, path := range paths {
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			lastErr = errRead
			continue
		}
		return snapshotFromBytes(path, raw)
	}
	snapshot := Snapshot{Path: paths[len(paths)-1]}
	if lastErr != nil {
		snapshot.Error = lastErr.Error()
	}
	return snapshot
}

// snapshotFromBytes parses raw. A file that was read stays readable even when it does
// not parse: the page needs to tell "no file" from "file is broken".
func snapshotFromBytes(path string, raw []byte) Snapshot {
	snapshot := Snapshot{Path: path, Readable: true}
	var document yaml.Node
	if errUnmarshal := yaml.Unmarshal(raw, &document); errUnmarshal != nil {
		snapshot.Error = errUnmarshal.Error()
		return snapshot
	}
	root := documentRoot(&document)
	if root == nil {
		return snapshot
	}

	plugins := mapValue(root, "plugins")
	snapshot.PluginsEnabled = boolPointer(mapValue(plugins, "enabled"))
	snapshot.PluginsDir = scalarString(mapValue(plugins, "dir"))

	logs := mapValue(mapValue(root, "observability"), "logs")
	snapshot.RequestLog = boolPointer(mapValue(logs, "request-log"))
	snapshot.LogsMaxTotalSizeMB = intPointer(mapValue(logs, "logs-max-total-size-mb"))

	snapshot.CommercialMode = boolPointer(mapValue(mapValue(root, "server"), "commercial-mode"))

	block := mapValue(mapValue(plugins, "configs"), buildinfo.ID)
	snapshot.PluginBlockKeys = mappingKeys(block)
	store := mapValue(block, "store")
	snapshot.StoreVersion = scalarString(mapValue(store, "version"))
	snapshot.StoreSource = scalarString(mapValue(store, "source-id"))
	return snapshot
}

// documentRoot unwraps a YAML document to its root node.
func documentRoot(document *yaml.Node) *yaml.Node {
	if document == nil || document.Kind != yaml.DocumentNode || len(document.Content) == 0 {
		return nil
	}
	return document.Content[0]
}

// mapValue returns the value node of key in a mapping node, or nil when the node is not
// a mapping or the key is absent. Only the key is looked up; the sibling values are
// never walked, which is what keeps credential values out of the snapshot.
func mapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

// mappingKeys lists the keys of a mapping node in file order. Only the key nodes are
// read, so a value (a key, a token) can never end up in the result.
func mappingKeys(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	keys := make([]string, 0, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		keys = append(keys, node.Content[index].Value)
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}

// scalarString returns the scalar value of a node, or "" when it is not a scalar.
func scalarString(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return ""
	}
	return node.Value
}

// boolPointer decodes a scalar node into a bool. A missing or non-scalar node stays nil
// so a missing key is not reported as false.
func boolPointer(node *yaml.Node) *bool {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return nil
	}
	var value bool
	if errDecode := node.Decode(&value); errDecode != nil {
		return nil
	}
	return &value
}

// intPointer decodes a scalar node into an int. Zero is a real value, so the pointer is
// set; only a missing or non-numeric node leaves it nil.
func intPointer(node *yaml.Node) *int {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return nil
	}
	var value int
	if errDecode := node.Decode(&value); errDecode != nil {
		return nil
	}
	return &value
}
