# fuse

A Go library for user-space file systems, forked from `jacobsa/fuse`. The
module path is `github.com/dsfaccini/fuse`. Its consumer is
`github.com/cloudflare/artifact-fs`, developed in `dsfaccini/artifact-fs`.

## Tests never mount by default

- Every mounting test calls `mounttest.SkipUnlessEnabled` first, so plain
  `go test ./...` mounts nothing. The mounting tests run only with
  `FUSE_RUN_MOUNT_TESTS=1`.
- The FSKit probe calls `mounttest.SkipUnlessFSKitProbeEnabled` instead. See
  "The FSKit probe".
- Gate every test that mounts through FSKit with
  `mounttest.SkipUnlessFSKitProbeEnabled`. A test that calls
  `MountWithFakeMFMount` takes no gate, because the fake MFMount never reaches
  FSKit. The scanner checks the FSKit gate only for calls to
  `mountFSKitTransport`, not for `Mount` with `BackendFSKit`.
- `TestMountingTestsAreGated` in `internal/mounttest` enforces the gate. Its
  failure names the fix.
- That test matches names without type checking, so it can miss a mount
  reached through a shape it does not model, such as non-test code or another
  package. Read new mounting code by hand.
- Set `FUSE_RUN_MOUNT_TESTS=1` only on a host that can mount. On Linux that
  needs `fuse3`. On darwin the tests mount through the macFUSE kernel
  extension, which needs macFUSE installed with that extension enabled.
- Only the `linux-mount` CI job sets `FUSE_RUN_MOUNT_TESTS=1`. To check a
  mounting test, push and read that job.
- To test `Connection` without a mount, use `testTransport` in
  `transport_test.go`. It feeds canned messages to `ReadOp` and records or
  fails the writes `Reply` makes. `newOpTestConnection` in
  `short_reads_test.go` completes `Init` over it and drives ops through
  `ReadOp` and `Reply`.
- To pin that a path does not allocate, use `testing.AllocsPerRun`, as
  `transport_test.go` does. Take out-messages from `getOutMessage`. A zero
  `buffer.OutMessage` keeps no scratch, so each of its `Grow` calls allocates.
- `BenchmarkRoundTrip*` in `reply_bench_test.go` measure one request and its
  reply over `cannedTransport`. Run them with
  `-run '^$' -bench BenchmarkRoundTrip -benchmem -benchtime 200ms -count 5`.
  Trust their allocation counts. Their timings vary with the machine's load.
- `readDevice` maps device errnos, and `fskitTransport.ReadMessage` maps
  FSKit's. `testTransport` returns its errors unmapped, so test a device errno
  through `fakeKernel`.
- To check what the connection logs to `MountConfig.ErrorLogger` or
  `DebugLogger`, serve through `ServeFakeKernelWithLoggers` in
  `fake_kernel_test.go`.
- To test a `Server` without a mount, use `ServeCanned` in `export_test.go`.
  It serves the way `Mount` does and returns what `Join` returns.
- To test how an op is decoded and answered, add a case to `protocolCases` in
  `protocol_test.go`. Each case runs over a fake kernel transport on every OS,
  and through `Mount` and the fake MFMount on darwin.
- To test `Mount`'s config validation without a mount, pass a mount point that
  does not exist. `Mount` validates the config before it checks the mount
  point. A regression then fails with ENOENT instead of mounting.
- On darwin, the root package's test binary cannot reach macFUSE by default.
  Its `init` in `mount_deny_darwin_test.go` points `fskitFrameworkPath` at a
  missing path unless `FUSE_RUN_FSKIT_PROBE=1`, and `macfuseMountHelper`
  unless `FUSE_RUN_MOUNT_TESTS=1`. `TestMountsAreDeniedByDefault` checks it.
- To load the installed MFMount in a test, call `useInstalledMFMount`. It
  restores the denied path and the loader cache when the test ends.
- Before you run a mutant of mount, backend or loader code, check that every
  test it runs reaches only the fake MFMount, a fake mount helper or a missing
  path. If you cannot tell, do not run it. Packages other than the root have
  no default-deny.
- `Mount` force-unmounts a native mount it will not return, and
  `MountedFileSystem.Unmount` unmounts through `unmountBackend`. Both call
  `unmountSyscall`, and FSKit's EPERM fallback runs `diskutilPath`. A test
  that reaches them replaces those first: `recordUnmounts` in
  `unmount_test.go` records the unmount(2) calls and answers them.
- To test the macFUSE kernel extension path without a mount, call
  `useFakeMacFUSEMountHelper`. It points `macfuseMountHelper` at the test
  binary running `TestFakeMacFUSEMountHelper`, which `fakeMountHelper`
  scripts: the device it passes back, its mount result and its exit status.

## Validate a change

Run these locally, in this order. None of them mounts.

1. `gofmt -l .` prints nothing.
2. `go build ./...`
3. `go vet -composites=false ./...` with `GOOS=linux GOARCH=amd64` and
   `GOOS=linux GOARCH=arm64`. Add `-unsafeptr=false` with `GOOS=darwin`.
   composites reports unkeyed `fusekernel.Protocol` literals older than the
   fork. On darwin, unsafeptr reports the purego addresses that the FSKit fake
   and probe convert to pointers.
4. `go test ./...`. Add `-race` when the change touches concurrency.

When you change `convertInMessage`, also run
`go test -run '^$' -fuzz FuzzConvertInMessage -fuzztime 60s .`. Fix each
crash it finds. Keep the input file it writes under
`testdata/fuzz/FuzzConvertInMessage/`: `go test` replays that corpus.

CI (`.github/workflows/ci.yml`) runs on every pull request and every push to
`master`:

- `gofmt`.
- `linux` and `macos`: build, vet and `go test -race -count=1 ./...`.
- `linux` also runs `GOARCH=386 go test -count=1 ./...`. A 32-bit `int` catches
  wire lengths that overflow it. Go has no darwin/386 port. On a Mac, leave
  that step to CI.
- `linux-mount`: installs `fuse3` and runs
  `go test -race -count=1 -timeout 10m ./...` with `FUSE_RUN_MOUNT_TESTS=1`,
  twice. As the runner user, `Mount` falls back to `fusermount3`. As root
  through `sudo`, `Mount` mounts directly.
- `linux-mount` fails when no test that `mounttest.SkipUnlessEnabled` let run
  passed. It finds those tests by the line the gate logs.
- `macos` installs macFUSE 5.5.0 from its GitHub release, pinned by
  SHA-256. The tests that call `useInstalledMFMount` fail when
  `GITHUB_ACTIONS=true` and macFUSE is missing or older than 5.5, and skip
  elsewhere.
- `artifact-fs`: builds and vets `dsfaccini/artifact-fs` `main` against this
  checkout, for Linux and darwin. An API change that breaks the consumer fails
  here.

## On Linux, Mount answers the poll hack's requests itself

- Before `Mount` returns, it opens and polls `.fuse-poll-hack` in the mount's
  root, so the kernel stops sending FUSE_POLL. `poll_hack.go` says why.
- The connection answers that entry's requests: its lookup while `Mount` runs
  the hack, then every request for inode `math.MaxUint64`. A server never
  reads them, and the wire log never records them.
- A `/dev/fd/N` mount point skips the hack, so the deadlock and inode
  `math.MaxUint64` stay the file system's. A failed hack is only logged to
  `MountConfig.ErrorLogger`, and to nowhere when that is nil.
- Unless `MountConfig.DisableDefaultPermissions` is set, `Mount` returns only
  after the server answers a GETATTR for the root. A test server that blocks
  that answer hangs `Mount`.

## Find an op error's errno with `operror.Errno`

- An op error is the error a file system returns for an op.
- Call `Errno` in `internal/operror` to find its errno. Never call `errors.As`
  or `errors.Is` on an op error.
- Unwrapping a nil `*os.PathError` panics, and `Errno` recovers from that
  panic.
- The kernel reply, the error log and the wire log all judge the errno that
  `Errno` finds. When `Errno` finds none, the kernel gets EIO and the wire-log
  `Status` stays 0.

## Ops that return attributes feed the FSKit short-read check

- On FSKit, `Connection` replies EIO to a `ReadFileOp` reply that ends before
  both the read and the inode's size (macfuse#1231). That size is the larger of
  the sizes recorded when the read arrived and when it is answered.
- `short_reads.go` keeps a record for each inode the kernel holds through a
  lookup. Entry replies create records and count lookups. Getattr and setattr
  replies only update existing records. `trackReportedSizes` records growth
  before a reply is written, and `shrinkReportedSizes` records a shrink after
  the kernel has the reply. Each reply that can change a record holds the
  write side of `sizeReplyMu` from recording its growth, through its write, to
  recording its shrink, so records change in write order. A READ reply holds
  the read side from its check through its write. Forgets change records too,
  but write nothing and take neither side.
- An op whose reply gains an inode's attributes must report them in
  `forEachReportedSize`, with `lookup` true when the reply gives the kernel a
  lookup, and must be listed in `changesReportedSizes`. A missed size goes
  stale and fails reads inside it with EIO. A missed lookup lets a forget drop
  the record while the kernel still holds the inode.

## Wire structs have the host's layout

- Size a wire struct with `unsafe.Sizeof` of its `fusekernel` type. Never sum
  field widths by hand. On darwin, `Attr` carries crtime and flags, so `Attr`
  and `EntryOut` are 16 bytes longer than on Linux.
- Write a `fusekernel.Dirent` header as `fusekernel.DirentSize` bytes. Its
  trailing `Name [0]byte` pads `unsafe.Sizeof` past the 24-byte header.
- Check a layout against the C headers: Linux `include/uapi/linux/fuse.h`, and
  `include/fuse_kernel.h` in `macfuse/library`, macFUSE's libfuse.
  `fuseutil/dirent_test.go` parses READDIRPLUS replies by those headers.

## FSKit changes need a test that does not mount

Ship every change to `mount_fskit*.go` with a test that exercises the changed
code without mounting. CI never mounts through FSKit, so only such a test
covers the change.

- Use the fake MFMount in `mount_fskit_fake_darwin_test.go`. Its functions are
  purego callbacks, so the transport makes its real purego calls.
- `newFakeMFMount` activates a fake for one test, and records unmount(2) in
  `fakeMFMount.unmounts` instead of running it. Its cleanup fails the test
  unless every message is released once, and the channel once per reference:
  `MFChannelCreate`'s, plus one per `MFRetain`.
- Test transport methods on the transport that `fakeMFMount.transport` returns.
- For behavior that only `Mount` reaches, call `MountWithFakeMFMount`, which
  calls `MountContext` with the context you pass. Set `fakeMFMount.mount` to
  script what MFMount does and returns.
- Call `Mount` or `MountContext` with a fake through `MountWithFakeMFMount`
  only. The gate scanner exempts that name in the repo root and flags any
  other path to either.
- Queue requests with `fakeMFMount.kernel.Send` and read replies with
  `NextReply`. A request is a list of body buffers. FSKit sends a WRITE's
  payload in a buffer of its own.

## The FSKit probe

- `TestFSKitProbe` measures how macFUSE's FSKit backend treats the FUSE
  protocol. One run makes at most 5 FSKit mounts. Its doc comment lists the
  mounts and what each measures.
- Its gate is `FUSE_RUN_FSKIT_PROBE=1`. `FUSE_RUN_MOUNT_TESTS` does not run it.
- Run it only on David's go-ahead in his own words. A click on an option you
  wrote is not a go-ahead. macFUSE's FSKit extension switches itself off after
  a series of mounts (https://github.com/macfuse/macfuse/issues/1232), and a
  stalled FSKit mount clears only on reboot.
- Run it on darwin, outside any sandbox:
  ```
  FUSE_RUN_FSKIT_PROBE=1 GOWORK=off go test -tags fuse_record -run '^TestFSKitProbe$' -v -count=1 -timeout 30m .
  ```
- Each result is a log line that contains `FSKIT_PROBE ` and then JSON.
  `grep -o 'FSKIT_PROBE .*'` extracts them.
- `-tags fuse_record` writes each mount's message stream to
  `testdata/fskit/<mount>.frames`. `fskit_probe_record_darwin_test.go` defines
  the format.
- After the run, `mount | grep -E 'macfuse|fskit-probe'` must print nothing.
- `mount | grep -i fskit` also lists Apple's FSKit volumes, such as
  CoreDevice's DeviceFS. They are expected. Never unmount them.
- A killed test process can leave a probe volume behind. Remove it with
  `diskutil unmount force <dir>`, only for a mount point whose name starts with
  `fskit-probe-`.
- When you change the number of mounts, change `fskitProbeMounts` and the doc
  comment together. The probe fails before its first mount when its plan and
  `fskitProbeMounts` disagree.

## Keep this file current

- When a change teaches something the next agent needs, update this file in the
  same PR. That covers a trap, a test gate, a CI fact and a cheaper local check.
- Put a rule here only when no lint, test or CI check can enforce it.
