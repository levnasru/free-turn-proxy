// cmd/desktop/embed_darwin_arm64.go
//go:build darwin && arm64

package main

import _ "embed"

//go:embed embedded/darwin_arm64/client
var embeddedClient []byte

//go:embed embedded/darwin_arm64/xray
var embeddedXray []byte

// embeddedWintun is nil on macOS — there is no wintun.dll equivalent needed
// here; vk-turn (tun) is not supported on darwin yet (see netroute_darwin.go
// and elevate_darwin.go), so resolveXrayBin's Windows-only wintun.dll
// extraction branch never runs on this platform regardless.
var embeddedWintun []byte
