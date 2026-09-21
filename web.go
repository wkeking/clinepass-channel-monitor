package main

import _ "embed"

// indexPage is the management page. It is embedded so a deployment never depends on
// extra files next to the plugin binary.
//
//go:embed web/index.html
var indexPage []byte
