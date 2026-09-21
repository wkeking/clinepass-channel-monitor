// Command clinepass-channel-monitor builds a CLIProxyAPI (CPA) plugin shared library.
//
// The plugin records which upstream channel actually served each Cline subscription
// request, together with usage, cost and cache counters. It observes; it never
// modifies the traffic flowing through CPA.
package main

/*
#include "cdecl.h"

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}

int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
void cliproxyPluginFree(void*, size_t);
void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

// envelope is the RPC response wrapper shared by every plugin method.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
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
	// A panic must never cross the C ABI boundary: it would abort the CPA process.
	// Recover here so that a bug in this plugin can only ever disable the plugin.
	defer func() {
		if recovered := recover(); recovered != nil {
			ret = 1
			raw := errorEnvelope("plugin_panic", fmt.Sprintf("plugin recovered from panic: %v", recovered))
			writeResponse(response, raw)
			hostLogAsync("error", "clinepass-channel-monitor: recovered from panic", map[string]string{
				"method": dispatchMethodName(method),
				"panic":  fmt.Sprint(recovered),
				"stack":  trimStack(debug.Stack()),
			})
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	name := C.GoString(method)
	raw, errHandle := handleMethod(name, requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
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
	shutdown()
}

func dispatchMethodName(method *C.char) string {
	if method == nil {
		return ""
	}
	return C.GoString(method)
}

func trimStack(stack []byte) string {
	const maxStack = 1024
	if len(stack) > maxStack {
		return string(stack[:maxStack])
	}
	return string(stack)
}

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

func okEnvelope(result any) ([]byte, error) {
	raw := json.RawMessage("null")
	if result != nil {
		marshaled, errMarshal := json.Marshal(result)
		if errMarshal != nil {
			return nil, errMarshal
		}
		raw = marshaled
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
