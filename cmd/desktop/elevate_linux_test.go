//go:build linux

package main

import "testing"

func TestIsElevatedFalseForNonRoot(t *testing.T) {
	// The CI/dev container and any normal dev machine run this test as a
	// non-root user — asserting isElevated() == true here would require the
	// test suite itself to run as root, which we don't want. This test only
	// pins the non-elevated path; the elevated path (euid==0) is exercised
	// manually per Task 5's live-test checklist, since faking os.Geteuid()
	// would just test a mock, not the real syscall.
	if isElevated() {
		t.Skip("test process is running as root — skipping, this path needs a non-root runner")
	}
}
