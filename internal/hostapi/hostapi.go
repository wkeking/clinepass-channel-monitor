// Package hostapi is the plugin-side bridge to the CPA host callbacks (logging and the
// host data APIs such as the auth store). Every call is best effort: failures are swallowed
// so an observation can never influence the traffic it observes.
package hostapi

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
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/wkeking/clinepass-channel-monitor/internal/buildinfo"
)

// Store keeps the host API in this package's copy of the bridge storage. Every cgo file
// compiles its own copy of the preamble, so the entry point cannot fill it in from outside:
// it passes the pointer in as an unsafe.Pointer and this package casts it back.
func Store(host unsafe.Pointer) {
	if host == nil {
		return
	}
	C.store_host_api((*C.cliproxy_host_api)(host))
}

// callHost performs one host callback and returns the raw response bytes.
//
// Failures are swallowed on purpose: a plugin observation must never influence the
// traffic it observes.
func Call(method string, payload []byte) ([]byte, bool) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var req *C.uint8_t
	if len(payload) > 0 {
		req = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(req))
	}
	if C.call_host_api(cMethod, req, C.size_t(len(payload)), &response) != 0 {
		return nil, false
	}
	if response.ptr == nil || response.len == 0 {
		return nil, true
	}
	out := C.GoBytes(response.ptr, C.int(response.len))
	C.free_host_buffer(response.ptr, response.len)
	return out, true
}

const maxLogFieldLen = 512

// hostLog writes one structured line into the CPA log. It is best effort.
func Log(level, message string, fields map[string]string) {
	if len(message) == 0 {
		message = buildinfo.ID
	}
	trimmed := make(map[string]string, len(fields))
	for key, value := range fields {
		if len(value) > maxLogFieldLen {
			value = value[:maxLogFieldLen]
		}
		trimmed[key] = value
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"level":   level,
		"message": message,
		"fields":  trimmed,
	})
	if errMarshal != nil {
		return
	}
	_, _ = Call("host.log", payload)
}

// hostLogAsync emits a log line without blocking the caller.
func LogAsync(level, message string, fields map[string]string) {
	go Log(level, message, fields)
}
