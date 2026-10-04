package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"
)

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
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(1)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	defer func() {
		if r := recover(); r != nil {
			writeResponse(response, errorEnvelope("plugin_panic", fmt.Sprint(r)))
			rc = 1
		}
	}()
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	raw, ok := pluginCall(C.GoString(method), uint64(requestLen), func() []byte {
		if request == nil || requestLen == 0 {
			return nil
		}
		return C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	})
	writeResponse(response, raw)
	if !ok {
		return 1
	}
	return 0
}

// maxRequestBytes bounds one RPC envelope. Bodies are base64 in JSON, so a
// 32 MiB body (the compression limit) needs about 43 MiB of envelope.
const maxRequestBytes = 48 << 20

// pluginCall validates the request size before copying it out of C memory, so
// an oversized length can neither wrap C.int nor force a huge allocation.
func pluginCall(method string, requestLen uint64, read func() []byte) (raw []byte, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			raw, ok = errorEnvelope("plugin_panic", fmt.Sprint(r)), false
		}
	}()
	if requestLen > maxRequestBytes {
		return errorEnvelope("request_too_large", "request exceeds plugin size limit"), false
	}
	return dispatch(method, read(), handleMethod)
}

// dispatch runs a method handler and converts errors and panics into error
// envelopes. An unrecovered panic in a c-shared library aborts the host process.
func dispatch(method string, request []byte, handle func(string, []byte) ([]byte, error)) (raw []byte, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			raw, ok = errorEnvelope("plugin_panic", fmt.Sprint(r)), false
		}
	}()
	out, errHandle := handle(method, request)
	if errHandle != nil {
		return errorEnvelope("plugin_error", errHandle.Error()), false
	}
	return out, true
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { flushStats() }

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
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
