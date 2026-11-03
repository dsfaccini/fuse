// Copyright 2026 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fuse

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ebitengine/purego"
)

func TestFSKitOSVersion(t *testing.T) {
	unsupported := FSKitMountError{Result: FSKitMountResultUnsupportedOSVersion}
	for _, test := range []struct {
		version string
		local   bool
		want    error
	}{
		{version: "15.3", local: true, want: unsupported},
		{version: "15.4", local: true},
		{version: "15.7", want: unsupported},
		{version: "26.0"},
		{version: "26.0", local: true},
		{version: "27.2"},
	} {
		err := validateFSKitOSVersion(test.version, test.local)
		if !errors.Is(err, test.want) {
			t.Errorf("validateFSKitOSVersion(%q, local=%v) error = %v, want %v", test.version, test.local, err, test.want)
		}
	}

	err := validateFSKitOSVersion("not-a-version", false)
	if err == nil || errors.As(err, new(FSKitMountError)) {
		t.Errorf("validateFSKitOSVersion(%q) error = %v, want a parse error", "not-a-version", err)
	}
}

// setFSKitTestSystem points FSKit mounts at frameworkPath and reports
// osVersion as the running macOS version until the test ends.
func setFSKitTestSystem(t *testing.T, frameworkPath string, osVersion string) {
	t.Helper()
	oldPath, oldVersion := fskitFrameworkPath, fskitOSProductVersion
	fskitFrameworkPath = frameworkPath
	fskitOSProductVersion = func() (string, error) { return osVersion, nil }
	t.Cleanup(func() { fskitFrameworkPath, fskitOSProductVersion = oldPath, oldVersion })
}

func TestLoadFSKitAPIReportsMissingFramework(t *testing.T) {
	_, err := loadFSKitAPI(filepath.Join(t.TempDir(), "MFMount"))
	if !errors.Is(err, ErrFSKitUnavailable) {
		t.Fatalf("loadFSKitAPI error = %v, want ErrFSKitUnavailable", err)
	}
}

func TestLoadFSKitAPIResolvesInstalledFramework(t *testing.T) {
	if _, err := os.Stat(fskitFrameworkPath); err != nil {
		// CI installs macFUSE, so there a missing framework is a failure.
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("macFUSE MFMount framework not installed: %v", err)
		}
		t.Skipf("macFUSE MFMount framework not installed: %v", err)
	}
	if _, err := loadFSKitAPI(fskitFrameworkPath); err != nil {
		t.Fatalf("loadFSKitAPI(%q): %v", fskitFrameworkPath, err)
	}
}

func TestCallErrnoDropsGarbageUpperBits(t *testing.T) {
	// MFChannelCopyNextMessage reported this raw value for ENODEV when the
	// channel closed; without truncation the transport panicked on shutdown.
	if got := callErrno(0x6f18b00000000013); got != syscall.ENODEV {
		t.Fatalf("callErrno = %v, want ENODEV", got)
	}

	libc, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW)
	if err != nil {
		t.Fatal(err)
	}
	closeFn, err := purego.Dlsym(libc, "close")
	if err != nil {
		t.Fatal(err)
	}
	badFD := ^uintptr(0) // -1
	if _, _, raw := purego.SyscallN(closeFn, badFD); callErrno(raw) != syscall.EBADF {
		t.Fatalf("close(-1) errno = %v, want EBADF", callErrno(raw))
	}
}

func TestMountReturnsTypedFSKitErrors(t *testing.T) {
	// Every case uses a missing framework, so none can reach MFMount.
	missingFramework := filepath.Join(t.TempDir(), "MFMount")
	if _, err := loadFSKitAPI(missingFramework); !errors.Is(err, ErrFSKitUnavailable) {
		t.Fatalf("loadFSKitAPI(%q) error = %v, want ErrFSKitUnavailable", missingFramework, err)
	}
	config := &MountConfig{Options: map[string]string{"backend": "fskit"}}

	setFSKitTestSystem(t, missingFramework, "15.7")
	_, err := Mount(t.TempDir(), nil, config)
	var mountErr FSKitMountError
	if !errors.As(err, &mountErr) || mountErr.Result != FSKitMountResultUnsupportedOSVersion {
		t.Errorf("Mount on macOS 15.7 error = %v, want FSKitMountError with result %d", err, FSKitMountResultUnsupportedOSVersion)
	}

	setFSKitTestSystem(t, missingFramework, "26.0")
	if _, err := Mount(t.TempDir(), nil, config); !errors.Is(err, ErrFSKitUnavailable) {
		t.Errorf("Mount without MFMount error = %v, want ErrFSKitUnavailable", err)
	}
}

func TestFSKitTransportCloseWaitsForMountPublication(t *testing.T) {
	for _, test := range []struct {
		name      string
		succeeded bool
	}{
		{name: "native mount failed"},
		{name: "native mount succeeded", succeeded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var closes, releases atomic.Int32
			closeErr := errors.New("native close error")
			transport := &fskitTransport{
				closeNative: func() error {
					closes.Add(1)
					return closeErr
				},
				releaseNative: func() { releases.Add(1) },
			}
			// The native call has returned, but its result is not published yet.
			transport.mountDone.Add(1)
			results := make(chan error, 2)
			for range 2 {
				go func() { results <- transport.Close() }()
			}
			select {
			case err := <-results:
				t.Fatalf("Close returned before mount publication: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			transport.mu.Lock()
			closed := transport.closed
			transport.mu.Unlock()
			if closed || closes.Load() != 0 || releases.Load() != 0 {
				t.Error("Close blocked pending INIT traffic or touched the native channel before mount publication")
			}
			transport.setMountResult(test.succeeded)
			transport.mountDone.Done()
			var wantErr error
			var wantCloses int32
			if test.succeeded {
				wantErr, wantCloses = closeErr, 1
			}
			for range 2 {
				select {
				case err := <-results:
					if !errors.Is(err, wantErr) {
						t.Errorf("Close error = %v, want %v", err, wantErr)
					}
				case <-time.After(time.Second):
					t.Fatal("Close did not finish after mount publication")
				}
			}
			if closes.Load() != wantCloses || releases.Load() != 1 {
				t.Fatalf("close calls=%d release calls=%d, want %d and 1", closes.Load(), releases.Load(), wantCloses)
			}
		})
	}
}

func TestFSKitTransportCloseDrainsOperationsBeforeRelease(t *testing.T) {
	closed := make(chan struct{})
	released := make(chan struct{})
	transport := &fskitTransport{
		mountSucceeded: true,
		closeNative:    func() error { close(closed); return nil },
		releaseNative:  func() { close(released) },
	}
	transport.operations.Add(1)
	result := make(chan error, 1)
	go func() { result <- transport.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not close the successful mount's channel")
	}
	select {
	case <-released:
		t.Error("native channel released while an operation was active")
	default:
	}
	transport.operations.Done()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after its operation drained")
	}
	select {
	case <-released:
	default:
		t.Fatal("Close did not release the native channel")
	}
}
