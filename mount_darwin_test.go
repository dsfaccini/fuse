package fuse

import "testing"

func TestMacFUSEKernelExtensionLoadedReadsItsSysctl(t *testing.T) {
	old := macfuseKernelExtensionSysctl
	t.Cleanup(func() { macfuseKernelExtensionSysctl = old })

	macfuseKernelExtensionSysctl = "kern.osproductversion"
	if !MacFUSEKernelExtensionLoaded() {
		t.Error("MacFUSEKernelExtensionLoaded = false with its sysctl present")
	}
	macfuseKernelExtensionSysctl = "vfs.generic.macfuse.no.such.sysctl"
	if MacFUSEKernelExtensionLoaded() {
		t.Error("MacFUSEKernelExtensionLoaded = true with its sysctl missing")
	}
}
