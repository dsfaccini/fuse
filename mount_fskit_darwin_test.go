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
	"sync"
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

func TestFSKitTransportCloseState(t *testing.T) {
	for _, test := range []struct {
		name      string
		received  bool
		finished  bool
		succeeded bool
		wantClose bool
	}{
		{name: "awaiting first message", wantClose: false},
		{name: "first message proves open", received: true, wantClose: true},
		{name: "mount succeeded", finished: true, succeeded: true, wantClose: true},
		{name: "mount failed after first message", received: true, finished: true, wantClose: false},
		{name: "mount failed before first message", finished: true, wantClose: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &fskitTransport{
				receivedNativeMessage: test.received,
				mountFinished:         test.finished,
				mountSucceeded:        test.succeeded,
			}
			transport.stateChanged = sync.NewCond(&transport.mu)

			transport.mu.Lock()
			got := transport.claimNativeCloseLocked()
			transport.mu.Unlock()
			if got != test.wantClose {
				t.Errorf("claimNativeCloseLocked() = %v, want %v", got, test.wantClose)
			}
		})
	}
}

func TestFSKitTransportStateChangesWakeWaiters(t *testing.T) {
	for _, test := range []struct {
		name      string
		update    func(*fskitTransport)
		wantClose bool
	}{
		{name: "first native message", update: func(transport *fskitTransport) { transport.noteNativeMessage() }, wantClose: true},
		{name: "mount failure", update: func(transport *fskitTransport) { transport.setMountResult(false) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &fskitTransport{}
			transport.stateChanged = sync.NewCond(&transport.mu)
			result := make(chan bool, 1)
			go func() {
				transport.mu.Lock()
				for !transport.receivedNativeMessage && !transport.mountFinished {
					transport.stateChanged.Wait()
				}
				closeChannel := transport.shouldCloseNativeChannelLocked()
				transport.mu.Unlock()
				result <- closeChannel
			}()

			test.update(transport)
			select {
			case got := <-result:
				if got != test.wantClose {
					t.Errorf("close decision = %v, want %v", got, test.wantClose)
				}
			case <-time.After(time.Second):
				t.Fatal("state change did not wake Close waiter")
			}
		})
	}
}
