/*
Copyright 2026 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gc

import (
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
)

// TestSetupInterval covers the interval-resolution branches that return
// before touching the supplied manager, so a nil manager is safe to pass
// here. The "no option supplied" and "positive override" paths still call
// mgr.Add and are covered by manual/e2e verification instead, since they
// need a real manager.
func TestSetupInterval(t *testing.T) {
	cases := map[string]struct {
		opts    []Option
		wantErr bool
	}{
		"ZeroDisablesWithoutTouchingManager": {
			opts:    []Option{WithInterval(0)},
			wantErr: false,
		},
		"NegativeIsRejected": {
			opts:    []Option{WithInterval(-1 * time.Hour)},
			wantErr: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Setup(nil, "/tf", logging.NewNopLogger(), tc.opts...)

			if tc.wantErr && err == nil {
				t.Errorf("Setup(...): want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Setup(...): want no error, got %v", err)
			}
		})
	}
}
