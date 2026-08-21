/*
Copyright 2020 The Crossplane Authors.

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

package claims

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/test"
	"github.com/pkg/errors"
	coordinationv1 "k8s.io/api/coordination/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGuardDisabledIsPassthrough(t *testing.T) {
	// cfg.Enabled=false must never touch mgr at all -- a nil Manager must
	// not panic, proving the short-circuit happens before any claim I/O.
	cfg := Config{Enabled: false}

	var fnCalled bool
	err := Guard(context.Background(), nil, cfg, testWorkspace(""), testGVK,
		func(ctx context.Context) error { fnCalled = true; return nil },
	)
	if err != nil {
		t.Fatalf("Guard(...): unexpected error: %v", err)
	}
	if !fnCalled {
		t.Error("fn was not called")
	}
}

func TestGuardAcquiredRunsFnAndReleases(t *testing.T) {
	// A tiny fake backing store: Get/Update read and write the same Lease,
	// so Release's independent Get sees what Acquire's Update wrote.
	var stored coordinationv1.Lease
	c := &test.MockClient{
		MockGet: func(_ context.Context, _ client.ObjectKey, obj client.Object) error {
			l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
			stored.DeepCopyInto(l)
			return nil
		},
		MockUpdate: func(_ context.Context, obj client.Object, _ ...client.UpdateOption) error {
			l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
			l.DeepCopyInto(&stored)
			return nil
		},
	}
	mgr := NewManager(c, Config{HolderIdentity: "instance-1", Namespace: "provider-ns", TTL: 90 * time.Second})

	var fnCalled bool
	err := Guard(context.Background(), mgr, Config{Enabled: true, HeartbeatInterval: time.Hour}, testWorkspace(""), testGVK,
		func(ctx context.Context) error { fnCalled = true; return nil },
	)
	if err != nil {
		t.Fatalf("Guard(...): unexpected error: %v", err)
	}
	if !fnCalled {
		t.Error("fn was not called")
	}
	if stored.Spec.HolderIdentity != nil {
		t.Errorf("claim was not released after fn succeeded: HolderIdentity = %q", *stored.Spec.HolderIdentity)
	}
}

func TestGuardBackoffDoesNotRunFn(t *testing.T) {
	holder := "instance-2"
	fresh := time.Now()
	c := &test.MockClient{
		MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
			l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
			l.Spec.HolderIdentity = &holder
			mt := microTime(fresh)
			l.Spec.RenewTime = mt
			return nil
		}),
	}
	mgr := NewManager(c, Config{HolderIdentity: "instance-1", Namespace: "provider-ns", TTL: 90 * time.Second})

	err := Guard(context.Background(), mgr, Config{Enabled: true, HeartbeatInterval: time.Hour}, testWorkspace(""), testGVK,
		func(ctx context.Context) error { t.Fatal("fn must not run while a foreign claim is fresh"); return nil },
	)
	if !errors.Is(err, ErrBackoff) {
		t.Errorf("err = %v, want ErrBackoff", err)
	}
}

// stolenClaimManager returns a Manager whose next Acquire steals a stale,
// foreign claim -- the R4 path.
func stolenClaimManager() *Manager {
	holder := "instance-2"
	stale := time.Now().Add(-100 * time.Second)
	c := &test.MockClient{
		MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
			l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
			l.Spec.HolderIdentity = &holder
			mt := microTime(stale)
			l.Spec.RenewTime = mt
			return nil
		}),
		// Acquire's steal path writes the new holder via Update; this must
		// succeed for Stolen to be returned at all.
		MockUpdate: test.NewMockUpdateFn(nil),
	}
	return NewManager(c, Config{HolderIdentity: "instance-1", Namespace: "provider-ns", TTL: 90 * time.Second})
}

func TestGuardStolenRunsFn(t *testing.T) {
	var fnCalled bool
	err := Guard(context.Background(), stolenClaimManager(), Config{Enabled: true, HeartbeatInterval: time.Hour}, testWorkspace(""), testGVK,
		func(ctx context.Context) error { fnCalled = true; return nil },
	)
	if err != nil {
		t.Fatalf("Guard(...): unexpected error: %v", err)
	}
	if !fnCalled {
		t.Error("fn was not called after the stale claim was stolen")
	}
}

func TestGuardStolenSurfacesLockError(t *testing.T) {
	// stealing a stale claim must not clear the state lock the
	// presumed-dead owner left behind. fn therefore runs straight into
	// Terraform's lock error, and Guard must hand that error back so it
	// reaches the Workspace's Synced condition instead of being swallowed.
	wantErr := errors.New("Error acquiring the state lock")
	err := Guard(context.Background(), stolenClaimManager(), Config{Enabled: true, HeartbeatInterval: time.Hour}, testWorkspace(""), testGVK,
		func(ctx context.Context) error { return wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}

func TestGuardFnErrorTakesPrecedenceOverReleaseError(t *testing.T) {
	// First Update (Acquire) succeeds; second Update (Release) fails -- Guard
	// must still surface fn's error, not Release's.
	var calls int32
	c := &test.MockClient{
		MockGet: test.NewMockGetFn(nil),
		MockUpdate: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
			if atomic.AddInt32(&calls, 1) == 1 {
				return nil
			}
			return kerrors.NewConflict(coordinationv1.Resource("leases"), "uid-a", errors.New("conflict"))
		},
	}
	mgr := NewManager(c, Config{HolderIdentity: "instance-1", Namespace: "provider-ns", TTL: 90 * time.Second})

	fnErr := errors.New("apply failed")
	err := Guard(context.Background(), mgr, Config{Enabled: true, HeartbeatInterval: time.Hour}, testWorkspace(""), testGVK,
		func(ctx context.Context) error { return fnErr },
	)
	if !errors.Is(err, fnErr) {
		t.Errorf("err = %v, want the fn error (%v), not the release error", err, fnErr)
	}
}

func TestGuardHeartbeatsWhileFnRuns(t *testing.T) {
	// A fake backing store, as in TestGuardAcquiredRunsFnAndReleases: each
	// heartbeat's Get must observe what the prior Update (acquire, or an
	// earlier heartbeat) wrote, or Heartbeat bails out as "not the holder".
	var stored coordinationv1.Lease
	var updates int32
	c := &test.MockClient{
		MockGet: func(_ context.Context, _ client.ObjectKey, obj client.Object) error {
			l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
			stored.DeepCopyInto(l)
			return nil
		},
		MockUpdate: func(_ context.Context, obj client.Object, _ ...client.UpdateOption) error {
			atomic.AddInt32(&updates, 1)
			l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
			l.DeepCopyInto(&stored)
			return nil
		},
	}
	mgr := NewManager(c, Config{HolderIdentity: "instance-1", Namespace: "provider-ns", TTL: 90 * time.Second})

	err := Guard(context.Background(), mgr, Config{Enabled: true, HeartbeatInterval: 5 * time.Millisecond}, testWorkspace(""), testGVK,
		func(ctx context.Context) error { time.Sleep(40 * time.Millisecond); return nil },
	)
	if err != nil {
		t.Fatalf("Guard(...): unexpected error: %v", err)
	}
	// The first Update acquires the claim and the last releases it;
	// sleeping 8x the interval in between should yield at least one
	// genuine heartbeat beyond those two.
	if atomic.LoadInt32(&updates) < 3 {
		t.Errorf("Update called %d times, want at least 3 (acquire + >=1 heartbeat + release)", updates)
	}
}
