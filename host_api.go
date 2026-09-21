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
*/
import "C"

import (
	"encoding/json"
	"unsafe"
)

// callHost performs one host callback and returns the raw response bytes.
//
// Failures are swallowed on purpose: a plugin observation must never influence the
// traffic it observes.
func callHost(method string, payload []byte) ([]byte, bool) {
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
func hostLog(level, message string, fields map[string]string) {
	if len(message) == 0 {
		message = pluginID
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
	_, _ = callHost("host.log", payload)
}

// hostLogAsync emits a log line without blocking the caller.
func hostLogAsync(level, message string, fields map[string]string) {
	go hostLog(level, message, fields)
}
