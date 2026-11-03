# fuse

A Go library for user-space file systems, forked from `jacobsa/fuse`. The
module path is `github.com/dsfaccini/fuse`. Its consumer is
`github.com/cloudflare/artifact-fs`, developed in `dsfaccini/artifact-fs`.

## Tests never mount by default

- Plain `go test ./...` never mounts a file system. Tests that mount skip unless
  `FUSE_RUN_MOUNT_TESTS=1`.
- `TestMountingTestsAreGated` in `internal/mounttest` fails on a mounting test
  without the gate. Its message names the fix.
- Set `FUSE_RUN_MOUNT_TESTS=1` only on a host that can mount: Linux with
  `fuse3`, or darwin with the macFUSE kernel extension loaded.
- No CI job sets `FUSE_RUN_MOUNT_TESTS` yet.

## Validate a change

Run these locally, in this order. None of them mounts.

1. `gofmt -l .` prints nothing.
2. `go build ./...`
3. `go vet -composites=false -unsafeptr=false ./...` with `GOOS=linux
   GOARCH=amd64`, `GOOS=linux GOARCH=arm64` and `GOOS=darwin`. The two disabled
   analyzers report findings older than the fork.
4. `go test ./...`. Add `-race` when the change touches concurrency.

CI (`.github/workflows/ci.yml`) runs on every pull request and every push to
`master`:

- `gofmt`.
- `linux` and `macos`: build, vet and `go test -race ./...`. `macos` installs
  macFUSE, so the FSKit tests load the real MFMount framework.
- `artifact-fs`: builds and vets `dsfaccini/artifact-fs` `main` against this
  checkout, for Linux and darwin. It runs without build tags, and again with
  `fskit` while artifact-fs still has that tag. An API change that breaks the
  consumer fails here.

## FSKit code needs a fake MFMount

CI never mounts through FSKit. Cover every change to `mount_fskit*.go` with a
test that drives the changed code through a fake MFMount. The fake does not
exist yet. Until it exists, do not change `mount_fskit*.go`.

## Keep this file current

- When a change teaches something the next agent needs, update this file in the
  same PR. That covers a trap, a test gate, a CI fact and a cheaper local check.
- Put a rule here only when no lint, test or CI check can enforce it.
