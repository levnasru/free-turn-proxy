// cmd/desktop/embed_linux.go
//go:build linux

package main

import _ "embed"

//go:embed embedded/linux_amd64/client
var embeddedClient []byte

//go:embed embedded/linux_amd64/xray
var embeddedXray []byte

// embeddedWintun is nil on Linux — xray's tun inbound only needs wintun.dll
// on Windows (see xray-core's proxy/tun README). Declared here too (not just
// in embed_windows.go) so resolveXrayBin in launcher.go can reference it
// unconditionally without a second build-tag split.
var embeddedWintun []byte
