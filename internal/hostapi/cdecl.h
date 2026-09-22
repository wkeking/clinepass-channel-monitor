/* C 侧类型声明，供 cgo 前置（// #include "cdecl.h"）使用。
 *
 * cgo 对相对路径 include 的解析依赖包的构建目录，跨包引用并不可靠，所以入口包
 * （cmd/clinepass-channel-monitor）与宿主回调包（internal/hostapi）各自持有一份内容
 * 完全相同的副本。改动时必须同时更新两处。
 */
/* Shared C ABI declarations for the CLIProxyAPI plugin host.
 *
 * Types only: each cgo file compiles its own copy of the preamble, so this header must
 * not define storage or functions. Every Go file that needs the host bridge declares
 * these as "static" locally, which keeps the symbols file-local and collision-free.
 */
#ifndef CLINEPASS_CHANNEL_MONITOR_CDECL_H
#define CLINEPASS_CHANNEL_MONITOR_CDECL_H

#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
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

#endif /* CLINEPASS_CHANNEL_MONITOR_CDECL_H */
