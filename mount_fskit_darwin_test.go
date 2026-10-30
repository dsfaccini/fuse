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
//go:build darwin && cgo && fskit

package fuse

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestFSKitOSVersion(t *testing.T) {
	for _, test := range []struct {
		version string
		wantErr bool
	}{
		{version: "15.3", wantErr: true},
		{version: "15.4", wantErr: false},
		{version: "26.0", wantErr: false},
		{version: "not-a-version", wantErr: true},
	} {
		err := validateFSKitOSVersion(test.version)
		if (err != nil) != test.wantErr {
			t.Errorf("validateFSKitOSVersion(%q) error = %v, wantErr %v", test.version, err, test.wantErr)
		}
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
