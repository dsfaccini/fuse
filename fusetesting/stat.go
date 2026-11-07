// Copyright 2015 Google Inc. All Rights Reserved.
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

package fusetesting

import (
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/jacobsa/oglematchers"
)

// Match os.FileInfo values that specify an mtime within d of the given time.
func MtimeIsWithin(expected time.Time, d time.Duration) oglematchers.Matcher {
	return oglematchers.NewMatcher(
		func(c interface{}) error { return mtimeIsWithin(c, expected, d) },
		fmt.Sprintf("mtime is within %v of %v", d, expected))
}

func mtimeIsWithin(c interface{}, expected time.Time, d time.Duration) error {
	fi, ok := c.(os.FileInfo)
	if !ok {
		return fmt.Errorf("which is of type %v", reflect.TypeOf(c))
	}

	// Check ModTime().
	diff := fi.ModTime().Sub(expected)
	absDiff := diff
	if absDiff < 0 {
		absDiff = -absDiff
	}

	if !(absDiff < d) {
		return fmt.Errorf("which has mtime %v, off by %v", fi.ModTime(), diff)
	}

	return nil
}
