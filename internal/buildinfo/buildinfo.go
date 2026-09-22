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
	Description = "逐条记录 Cline 请求实际命中的上游渠道、用量与成本，并提供管理页。"
)

// Version is stamped at build time:
//
//	-ldflags "-X github.com/wkeking/clinepass-channel-monitor/internal/buildinfo.Version=..."
var Version = "0.1.0"
