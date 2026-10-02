// Package buildinfo carries the plugin identity used by the host, the logs and the pages.
package buildinfo

const (
	// ID is the plugin id. It has to match the plugins.configs key and the shared library
	// file name CPA scans for.
	ID   = "clinepass-channel-monitor"
	Name = "Cline 渠道监控"
	// Author and Repository are shown by the management console.
	Author     = "wkeking"
	Repository = "https://github.com/wkeking/clinepass-channel-monitor"
	// Description is the one-line summary of the plugin.
	Description = "展示 Cline 套餐、限额与官方用量，并观测请求最终落在哪个渠道。"
)

// Version is stamped at build time:
//
//	-ldflags "-X github.com/wkeking/clinepass-channel-monitor/internal/buildinfo.Version=..."
var Version = "0.3.0"
