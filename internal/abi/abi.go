// Package abi implements the response envelope of the plugin ABI, including the "no change"
// answer that every observation hook returns.
package abi

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Envelope is the RPC response wrapper shared by every plugin method.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is the failure payload of an envelope.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// OK wraps a successful result.
func OK(result any) ([]byte, error) {
	raw := json.RawMessage("null")
	if result != nil {
		marshaled, errMarshal := json.Marshal(result)
		if errMarshal != nil {
			return nil, errMarshal
		}
		raw = marshaled
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

// Failure builds a failure envelope.
func Failure(code, message string) []byte {
	raw, _ := json.Marshal(Envelope{OK: false, Error: &Error{Code: code, Message: message}})
	return raw
}

// EmptyObservation is what a plugin answers when it does not want to change anything: the
// host treats an empty body as "keep the current payload". Every observation hook of this
// plugin returns exactly this, which is why it never alters traffic.
var EmptyObservation = func() []byte {
	raw, errMarshal := OK(pluginapi.PayloadResponse{})
	if errMarshal != nil {
		return Failure("encode_failed", errMarshal.Error())
	}
	return raw
}()
