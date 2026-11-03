# fuse

A Go library for user-space file systems, forked from `jacobsa/fuse`. The
module path is `github.com/dsfaccini/fuse`. Its consumer is
`github.com/cloudflare/artifact-fs`, developed in `dsfaccini/artifact-fs`.

## Tests never mount by default

- Every mounting test calls `mounttest.SkipUnlessEnabled` first, so plain
  `go test ./...` mounts nothing. The mounting tests run only with
  `FUSE_RUN_MOUNT_TESTS=1`.
- `TestMountingTestsAreGated` in `internal/mounttest` enforces the gate. Its
  failure names the fix.
- That test matches names without type checking, so it can miss a mount
  reached through a shape it does not model, such as non-test code or another
  package. Read new mounting code by hand.
- Set `FUSE_RUN_MOUNT_TESTS=1` only on a host that can mount. On Linux that
  needs `fuse3`. On darwin the tests mount through FUSE-T, the default
  `MountConfig.FuseImpl`, which needs `go-nfsv4` at `/usr/local/bin/go-nfsv4`
  or at `$FUSE_NFSSRV_PATH`.
- No CI job sets `FUSE_RUN_MOUNT_TESTS` yet.
- To test `Connection` without a mount, use `testTransport` in
  `transport_test.go`. It feeds canned messages to `ReadOp` and records or
  fails the writes `Reply` makes.

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
- `linux` and `macos`: build, vet and `go test -race -count=1 ./...`.
- `macos` installs macFUSE for `TestLoadFSKitAPIResolvesInstalledFramework`.
  That test loads the installed MFMount framework. It fails when
  `GITHUB_ACTIONS=true` and the framework is missing, and skips elsewhere.
- `artifact-fs`: builds and vets `dsfaccini/artifact-fs` `main` against this
  checkout, for Linux and darwin. An API change that breaks the consumer fails
  here.

## Find an op error's errno with `operror.Errno`

- An op error is the error a file system returns for an op.
- Call `Errno` in `internal/operror` to find its errno. Never call `errors.As`
  or `errors.Is` on an op error.
- Unwrapping a nil `*os.PathError` panics, and `Errno` recovers from that
  panic.
- The kernel reply, the error log and the wire log all judge the errno that
  `Errno` finds. When `Errno` finds none, the kernel gets EIO and the wire-log
  `Status` stays 0.

## FSKit changes need a test that does not mount

Ship every change to `mount_fskit*.go` with a test that exercises the changed
code without mounting. CI never mounts through FSKit, so only such a test
covers the change.

## Keep this file current

- When a change teaches something the next agent needs, update this file in the
  same PR. That covers a trap, a test gate, a CI fact and a cheaper local check.
- Put a rule here only when no lint, test or CI check can enforce it.
