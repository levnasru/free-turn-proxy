//go:build windows

package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// isElevated reports whether this process's token already carries
// administrator rights — Windows' precondition for creating a wintun
// adapter.
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

var (
	shell32           = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteW = shell32.NewProc("ShellExecuteW")
)

// relaunchElevated re-launches the current binary with the "runas" verb,
// which triggers the UAC consent prompt, passing extraArgs as a single
// space-joined command line. It returns as soon as the new process has been
// launched (or the UAC prompt was declined) — runas starts a detached
// process, so the caller should NOT wait on it; the elevated instance is now
// running independently and this (unprivileged) instance's job is done.
func relaunchElevated(extraArgs []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("relaunchElevated: resolve self path: %w", err)
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(self)
	params, _ := syscall.UTF16PtrFromString(strings.Join(extraArgs, " "))
	dir, _ := syscall.UTF16PtrFromString("")
	const swShowNormal = 1
	ret, _, _ := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)),
		uintptr(unsafe.Pointer(dir)),
		swShowNormal,
	)
	// ShellExecuteW returns a value > 32 on success; anything <= 32 is an
	// HINSTANCE-shaped error code (e.g. 5 = ERROR_ACCESS_DENIED when the
	// user declines the UAC prompt). See Win32 ShellExecute docs.
	if ret <= 32 {
		return fmt.Errorf("relaunchElevated: ShellExecuteW failed, code %d", ret)
	}
	return nil
}
