// cmd/desktop/embed_darwin_amd64.go
//go:build darwin && amd64

package main

import _ "embed"

//go:embed embedded/darwin_amd64/client
var embeddedClient []byte

//go:embed embedded/darwin_amd64/xray
var embeddedXray []byte

// embeddedWintun is nil on macOS — there is no wintun.dll equivalent needed
// here; vk-turn (tun) is not supported on darwin yet (see netroute_darwin.go
// and elevate_darwin.go), so resolveXrayBin's Windows-only wintun.dll
// extraction branch never runs on this platform regardless.
var embeddedWintun []byte
