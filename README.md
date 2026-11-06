[![ci](https://github.com/dsfaccini/fuse/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/dsfaccini/fuse/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dsfaccini/fuse.svg)](https://pkg.go.dev/github.com/dsfaccini/fuse)

This package allows for writing and mounting user-space file systems from Go.

It is a fork of [jacobsa/fuse][upstream]. Its module path is
`github.com/dsfaccini/fuse`. Install it as follows:

    go get github.com/dsfaccini/fuse

Afterward, see the documentation for the following three packages:

 *  Package [fuse][] provides support for mounting a new file system and
    reading requests from the kernel.

 *  Package [fuseops][] enumerates the supported requests from the kernel, and
    provides documentation on their semantics.

 *  Package [fuseutil][], in particular the `FileSystem` interface, provides a
    convenient way to create a file system type and export it to the kernel via
    `fuse.Mount`.

Make sure to also see the sub-packages of the [samples][] package for examples
and tests.

## Backends

`MountConfig.Backend` picks how the kernel reaches the file system. Its zero
value is the platform's default. `Mount` rejects a backend the platform lacks.

| Backend | Platform | Needs |
|---|---|---|
| `BackendLinux` | Linux, default | The kernel's FUSE module. Without the privilege to mount directly, `Mount` runs `fusermount3` or `fusermount`. |
| `BackendMacFUSEKernelExtension` | macOS, default | [macFUSE][macfuse] with its kernel extension allowed. |
| `BackendFSKit` | macOS | macFUSE 5.5 or later, and macOS 26 or later. With the `local` mount option, macOS 15.4 or later. |

For `BackendFSKit`, `Mount` checks before it mounts:

 *  The macOS version. It refuses an older one with an `FSKitMountError` whose
    result is `FSKitMountResultUnsupportedOSVersion`.

 *  The version of the installed macFUSE. It refuses an older one with an
    error that matches `ErrFSKitUnavailable`.

 *  That macFUSE's MFMount framework loads from its install path.

`FSKitAvailable` runs the same checks without mounting, for a mount without the
`local` option. It cannot see whether macFUSE's file system extension is
enabled. `Mount` reports that as an `FSKitMountError`.

This package owes its inspiration and most of its kernel-related code to
[bazil.org/fuse][bazil].

[upstream]: https://github.com/jacobsa/fuse
[macfuse]: https://macfuse.github.io/
[fuse]: https://pkg.go.dev/github.com/dsfaccini/fuse
[fuseops]: https://pkg.go.dev/github.com/dsfaccini/fuse/fuseops
[fuseutil]: https://pkg.go.dev/github.com/dsfaccini/fuse/fuseutil
[samples]: https://pkg.go.dev/github.com/dsfaccini/fuse/samples
[bazil]: https://pkg.go.dev/bazil.org/fuse
