// Command channel-probe builds a throwaway CLIProxyAPI (CPA) diagnostic plugin.
//
// It answers every request-side hook with the "no change" envelope and only records the raw
// payload the host handed over, to learn whether the selected upstream credential (auth id,
// provider, channel) is visible to a plugin whatever the client wire protocol.
// Not production code: every failure is swallowed so a broken probe can only lose its own line.
package main

/*
#include "cdecl.h"

int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
void cliproxyPluginFree(void*, size_t);
void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/wkeking/clinepass-channel-monitor/internal/abi"
)

// Identity inlined instead of internal/buildinfo: this is not a release artifact and must not
// inherit the real plugin's version stamp.
const (
	probeName        = "channel-probe"
	probeVersion     = "0.1.0"
	probeAuthor      = "wkeking"
	probeRepo        = "https://github.com/wkeking/clinepass-channel-monitor"
	defaultProbePath = "/CLIProxyAPI/logs/channel-probe.jsonl" // CHANNEL_PROBE_OUT overrides it
	maxHits          = 40                                      // keeps one log line readable
	maxHitValue      = 200
	maxHitDepth      = 12 // a pathological payload must not become a full deep walk
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (ret C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	// A panic must never cross the C ABI boundary: it would abort the CPA process. Recovering
	// downgrades a probe bug to a failed call the host can log and move past.
	defer func() {
		if recovered := recover(); recovered != nil {
			ret = 1
			writeResponse(response, abi.Failure("plugin_panic", fmt.Sprintf("probe recovered from panic: %v", recovered)))
		}
	}()
	if method == nil {
		writeResponse(response, abi.Failure("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, abi.Failure("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = len
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	probeMu.Lock()
	defer probeMu.Unlock()
	if probeFile != nil {
		_ = probeFile.Close() // the log has no other owner, so shutdown releases it
		probeFile = nil
	}
}

// handleMethod sits outside the cgo boundary so a plain test or harness can drive the same
// paths without loading the shared library.
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return abi.OK(buildProbeRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return abi.OK(nil)
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		recordProbeLine(method, request)
		// The empty observation verbatim keeps the probe invisible: the host reads it as
		// "keep the payload you already have". It is the exact answer the production plugin
		// sends on its only hook.
		return abi.EmptyObservation, nil
	case pluginabi.MethodRequestComplete, pluginabi.MethodUsageHandle:
		// usage.handle carries the terminal UsageRecord: provider/auth id/auth index, model and
		// alias, tokens, TTFT, latency, failure status — one record per request, whatever the
		// client wire protocol was. That is the payload this probe exists to see. Both of these
		// methods answer an empty result on the host side, so the probe must not invent fields.
		recordProbeLine(method, request)
		return abi.OK(nil)
	default:
		return abi.Failure("unknown_method", "unknown method: "+method), nil
	}
}

// probeCapabilities declares exactly the two request hooks this diagnostic listens on. Every
// other capability stays omitted, which the host reads as false; management_api must stay false
// because the probe registers no management route.
type probeCapabilities struct {
	RequestInterceptor     bool `json:"request_interceptor"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	UsagePlugin            bool `json:"usage_plugin"`
	ManagementAPI          bool `json:"management_api"`
}

// probeRegistration mirrors internal/plugin.buildRegistration so the host treats this
// throwaway exactly like the real plugin.
type probeRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  probeCapabilities  `json:"capabilities"`
}

// buildProbeRegistration is a function, not a second identifier named probeRegistration: a type
// and a function cannot share one package-level name.
func buildProbeRegistration() probeRegistration {
	return probeRegistration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             probeName,
			Version:          probeVersion,
			Author:           probeAuthor,
			GitHubRepository: probeRepo,
		},
		Capabilities: probeCapabilities{RequestInterceptor: true, RequestLifecyclePlugin: true, UsagePlugin: true},
	}
}

// probeLine is one JSONL record. Raw stays JSON rather than a quoted string so a record can be
// compared against the ABI contract field by field.
type probeLine struct {
	Time   string            `json:"time"`
	Method string            `json:"method"`
	Bytes  int               `json:"bytes"`
	Keys   []string          `json:"keys"`
	Raw    any               `json:"raw"`
	Hits   map[string]string `json:"hits,omitempty"`
}

var (
	probeMu   sync.Mutex
	probeFile *os.File
)

// recordProbeLine appends one line, failing open at every step: this is instrumentation and it
// must never break the host call it observes.
func recordProbeLine(method string, payload []byte) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false) // a payload's "<" must survive verbatim
	if errEncode := encoder.Encode(buildProbeLine(method, payload)); errEncode != nil {
		return
	}
	probeMu.Lock()
	defer probeMu.Unlock()
	if probeFile == nil {
		path := os.Getenv("CHANNEL_PROBE_OUT")
		if path == "" {
			path = defaultProbePath
		}
		file, errOpen := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if errOpen != nil {
			return // no log available: stay silent instead of failing the caller
		}
		probeFile = file // opened lazily, kept open for the process lifetime
	}
	_, _ = probeFile.Write(buffer.Bytes()) // one Write per record keeps lines whole
}

// buildProbeLine describes whatever the host handed over.
func buildProbeLine(method string, payload []byte) probeLine {
	trimmed := bytes.TrimSpace(payload)
	line := probeLine{
		Time:   time.Now().Format(time.RFC3339Nano),
		Method: method,
		Bytes:  len(payload),
		Keys:   []string{},
		Raw:    string(payload),
	}
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return line // not JSON: keep the raw text so the wire payload stays inspectable
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber() // keep counters exact instead of rounding them through float64
	_ = decoder.Decode(&decoded)
	line.Raw = json.RawMessage(trimmed)
	object, isObject := decoded.(map[string]any)
	if !isObject {
		return line // a bare array or scalar: the raw value says everything there is to say
	}
	for key := range object {
		line.Keys = append(line.Keys, key)
	}
	sort.Strings(line.Keys) // map order is random; a stable line diffs cleanly
	line.Hits = collectHits(object)
	return line
}

// hitKeywords are the key-name fragments that can carry the upstream credential. Matching is
// deliberately fuzzy: the goal is to discover where CPA puts the auth id, not to assert a
// schema we already believe in.
var hitKeywords = []string{
	"provider", "auth", "credential", "channel", "route", "account",
	"model", "endpoint", "status", "trace", "session",
	"token", "latency", "ttft", "cache", "executor", "base_url", "source",
}

// collectHits flattens every matching key path into "a.b[0].c" -> stringified value.
func collectHits(root map[string]any) map[string]string {
	hits := make(map[string]string)
	walkHits("", root, hits, 0)
	if len(hits) == 0 {
		return nil
	}
	return hits
}

func walkHits(path string, value any, hits map[string]string, depth int) {
	if len(hits) >= maxHits || depth > maxHitDepth {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			if hasHitKeyword(key) {
				hits[childPath] = hitValue(child)
			}
			walkHits(childPath, child, hits, depth+1)
		}
	case []any:
		for index, child := range typed {
			walkHits(fmt.Sprintf("%s[%d]", path, index), child, hits, depth+1)
		}
	}
}

func hasHitKeyword(key string) bool {
	lower := strings.ToLower(key)
	for _, keyword := range hitKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

// hitValue stringifies one matched value and caps it so a body or header dump cannot bloat the
// line. Cutting on a rune boundary keeps the record valid UTF-8.
func hitValue(value any) string {
	text := ""
	switch typed := value.(type) {
	case nil:
		text = "null"
	case string:
		text = typed
	case json.Number:
		text = typed.String()
	default:
		if raw, errMarshal := json.Marshal(value); errMarshal != nil {
			text = fmt.Sprint(value)
		} else {
			text = string(raw)
		}
	}
	if len(text) <= maxHitValue {
		return text
	}
	cut := maxHitValue
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// writeResponse hands a buffer back to the host, which frees it through cliproxyPluginFree.
func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
