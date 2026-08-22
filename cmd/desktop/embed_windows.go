// cmd/desktop/embed_windows.go
//go:build windows

package main

import _ "embed"

//go:embed embedded/windows_amd64/client.exe
var embeddedClient []byte

//go:embed embedded/windows_amd64/xray.exe
var embeddedXray []byte

//go:embed embedded/windows_amd64/wintun.dll
var embeddedWintun []byte
