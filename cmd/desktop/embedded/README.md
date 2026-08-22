# cmd/desktop/embedded/

`go:embed`-source for `vkturn-desktop`'s single-binary distribution (see
`docs/superpowers/specs/2026-08-22-desktop-single-binary-design.md`).

The five files under `linux_amd64/` and `windows_amd64/` here are **tracked
placeholders**, not real binaries — `go:embed` requires the referenced file
to exist at build time, so a fresh clone needs *something* here or every
`go build ./...`/`go test ./...` in this repo breaks. `.github/workflows/release.yml`
overwrites them with the real `client`/`xray`/`wintun.dll` builds before the
release build of `cmd/desktop` runs, in its own ephemeral CI workspace.

**Never `git add`/commit a locally-overwritten real binary here.** If you run
a full two-stage build by hand for local testing, check `git status` before
committing anything — a real `client`/`xray` here is 15-33MB and does not
belong in this repository's history.
