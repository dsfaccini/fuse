# fuse

A Go library for user-space file systems, forked from `jacobsa/fuse`. The
module path is `github.com/dsfaccini/fuse`. Its consumer is
`github.com/cloudflare/artifact-fs`, developed in `dsfaccini/artifact-fs`.

## Tests never mount by default

- Plain `go test ./...` never mounts a file system. Tests that mount skip unless
  `FUSE_RUN_MOUNT_TESTS=1`.
- `TestMountingTestsAreGated` in `internal/mounttest` checks the test files of
  each directory. It flags a test, benchmark, fuzz target or example that calls
  `Mount`, ogletest's `RunTests` or a `SetUp` method without the gate. It also
  follows calls through functions declared in the same directory's test files.
  Its message names the fix.
- Set `FUSE_RUN_MOUNT_TESTS=1` only on a host that can mount. On Linux that
  needs `fuse3`. On darwin the tests mount through FUSE-T, the default
  `MountConfig.FuseImpl`, which needs `go-nfsv4` at `/usr/local/bin/go-nfsv4`
  or at `$FUSE_NFSSRV_PATH`.
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
- `linux` and `macos`: build, vet and `go test -race -count=1 ./...`. `macos` installs
  macFUSE, so the FSKit tests load the real MFMount framework.
- `artifact-fs`: builds and vets `dsfaccini/artifact-fs` `main` against this
  checkout, for Linux and darwin. An API change that breaks the consumer fails
  here.

## FSKit changes need a test that does not mount

Ship every change to `mount_fskit*.go` with a test that exercises the changed
code without mounting. CI never mounts through FSKit, so only such a test
covers the change.

## Keep this file current

- When a change teaches something the next agent needs, update this file in the
  same PR. That covers a trap, a test gate, a CI fact and a cheaper local check.
- Put a rule here only when no lint, test or CI check can enforce it.
