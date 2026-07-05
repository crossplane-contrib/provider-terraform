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
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/test"
	"github.com/pkg/errors"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var testGVK = schema.GroupVersionKind{Group: "tf.upbound.io", Version: "v1beta1", Kind: "Workspace"}

// testWorkspace returns a stand-in client.Object. The claims package has no
// dependency on either Workspace API, so any client.Object with a UID and
// (optionally) a namespace exercises it faithfully.
func testWorkspace(ns string) client.Object {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ws-a", Namespace: ns, UID: types.UID("uid-a")}}
}

func holderPtr(s string) *string { return &s }

func microTime(t time.Time) *metav1.MicroTime {
	mt := metav1.NewMicroTime(t)
	return &mt
}

func TestClaimKey(t *testing.T) {
	cfg := Config{Namespace: "provider-ns"}

	cases := map[string]struct {
		ws   client.Object
		want client.ObjectKey
	}{
		"ClusterScopedFallsBackToProviderNamespace": {
			ws:   testWorkspace(""),
			want: client.ObjectKey{Namespace: "provider-ns", Name: "uid-a"},
		},
		"NamespacedUsesWorkspaceNamespace": {
			// Keeps the Lease's owner reference same-namespace and therefore
			// valid -- Kubernetes silently drops a cross-namespace owner
			// reference and never garbage-collects across the boundary.
			ws:   testWorkspace("tenant-a"),
			want: client.ObjectKey{Namespace: "tenant-a", Name: "uid-a"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := claimKey(cfg, tc.ws); got != tc.want {
				t.Errorf("claimKey(...) = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAcquire(t *testing.T) {
	fixedNow := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	cfg := Config{HolderIdentity: "instance-1", Namespace: "provider-ns", TTL: 90 * time.Second}

	cases := map[string]struct {
		client  *test.MockClient
		want    Outcome
		wantErr bool
	}{
		"CreatesWhenAbsent": {
			// No Lease exists yet: this instance creates and claims it (R2).
			client: &test.MockClient{
				MockGet:    test.NewMockGetFn(kerrors.NewNotFound(coordinationv1.Resource("leases"), "uid-a")),
				MockCreate: test.NewMockCreateFn(nil),
			},
			want: Acquired,
		},
		"CreateRaceLoses": {
			// Two instances raced to create the Lease for a brand-new
			// Workspace; another instance's Create won (R2 loser).
			client: &test.MockClient{
				MockGet:    test.NewMockGetFn(kerrors.NewNotFound(coordinationv1.Resource("leases"), "uid-a")),
				MockCreate: test.NewMockCreateFn(kerrors.NewAlreadyExists(coordinationv1.Resource("leases"), "uid-a")),
			},
			want: Backoff,
		},
		"AcquiresEmptyClaim": {
			// Lease exists but is unclaimed (e.g. after a Release): take it.
			client: &test.MockClient{
				MockGet:    test.NewMockGetFn(nil),
				MockUpdate: test.NewMockUpdateFn(nil),
			},
			want: Acquired,
		},
		"ReacquiresOwnClaim": {
			// Already ours: re-acquiring (e.g. a retried reconcile) succeeds.
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
					l.Spec.HolderIdentity = holderPtr("instance-1")
					l.Spec.RenewTime = microTime(fixedNow)
					return nil
				}),
				MockUpdate: test.NewMockUpdateFn(nil),
			},
			want: Acquired,
		},
		"UpdateRaceLoses": {
			// Claim was empty when read, but another instance's write won
			// the resourceVersion race before ours landed (R2 loser).
			client: &test.MockClient{
				MockGet:    test.NewMockGetFn(nil),
				MockUpdate: test.NewMockUpdateFn(kerrors.NewConflict(coordinationv1.Resource("leases"), "uid-a", errors.New("conflict"))),
			},
			want: Backoff,
		},
		"ForeignFreshBacksOff": {
			// R3: a foreign, fresh (10s old, TTL 90s) claim -- do not run.
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
					l.Spec.HolderIdentity = holderPtr("instance-2")
					l.Spec.RenewTime = microTime(fixedNow.Add(-10 * time.Second))
					return nil
				}),
				MockUpdate: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
					t.Fatal("Update must not be called when a foreign claim is fresh (R3)")
					return nil
				},
			},
			want: Backoff,
		},
		"ForeignStaleIsStolen": {
			// R4: a foreign claim 100s old (> 90s TTL) is presumed abandoned.
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
					l.Spec.HolderIdentity = holderPtr("instance-2")
					l.Spec.RenewTime = microTime(fixedNow.Add(-100 * time.Second))
					return nil
				}),
				MockUpdate: test.NewMockUpdateFn(nil),
			},
			want: Stolen,
		},
		"MissingRenewTimeIsTreatedAsStale": {
			// A held claim with no renewTime yet must not be treated as
			// fresh -- isFresh returns false whenever RenewTime is nil.
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
					l.Spec.HolderIdentity = holderPtr("instance-2")
					return nil
				}),
				MockUpdate: test.NewMockUpdateFn(nil),
			},
			want: Stolen,
		},
		"GetErrorIsBackoff": {
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(errors.New("boom")),
			},
			want:    Backoff,
			wantErr: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := NewManager(tc.client, cfg)
			m.now = func() time.Time { return fixedNow }

			got, err := m.Acquire(context.Background(), testWorkspace(""), testGVK)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Acquire(...): err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("Acquire(...) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAcquireSetsOwnerReference(t *testing.T) {
	fixedNow := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	cfg := Config{HolderIdentity: "instance-1", Namespace: "provider-ns", TTL: 90 * time.Second}

	var created *coordinationv1.Lease
	c := &test.MockClient{
		MockGet: test.NewMockGetFn(kerrors.NewNotFound(coordinationv1.Resource("leases"), "uid-a")),
		MockCreate: test.NewMockCreateFn(nil, func(obj client.Object) error {
			created = obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
			return nil
		}),
	}
	m := NewManager(c, cfg)
	m.now = func() time.Time { return fixedNow }

	ws := testWorkspace("tenant-a")
	if _, err := m.Acquire(context.Background(), ws, testGVK); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if created == nil {
		t.Fatal("Create was not called")
	}
	if created.Namespace != "tenant-a" {
		t.Errorf("Lease namespace = %q, want %q (same-namespace as the Workspace, for a valid owner reference)", created.Namespace, "tenant-a")
	}
	if len(created.OwnerReferences) != 1 {
		t.Fatalf("got %d owner references, want 1", len(created.OwnerReferences))
	}
	if got := created.OwnerReferences[0].UID; got != ws.GetUID() {
		t.Errorf("owner reference UID = %q, want %q", got, ws.GetUID())
	}
	if got := created.OwnerReferences[0].Kind; got != testGVK.Kind {
		t.Errorf("owner reference Kind = %q, want %q", got, testGVK.Kind)
	}
}

func TestHeartbeat(t *testing.T) {
	fixedNow := time.Date(2026, 7, 3, 12, 5, 0, 0, time.UTC)
	cfg := Config{HolderIdentity: "instance-1", Namespace: "provider-ns", TTL: 90 * time.Second}

	cases := map[string]struct {
		client  *test.MockClient
		wantErr bool
	}{
		"RenewsOwnClaim": {
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
					l.Spec.HolderIdentity = holderPtr("instance-1")
					l.Spec.RenewTime = microTime(fixedNow.Add(-30 * time.Second))
					return nil
				}),
				MockUpdate: test.NewMockUpdateFn(nil),
			},
		},
		"ZombieWindowAbortsWhenClaimWasStolen": {
			// Design §8 residual risk: a paused-then-resumed owner may
			// briefly believe it still holds the claim. Its heartbeat must
			// fail once another instance has taken over, so it aborts
			// instead of running concurrently.
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
					l.Spec.HolderIdentity = holderPtr("instance-2")
					l.Spec.RenewTime = microTime(fixedNow)
					return nil
				}),
				MockUpdate: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
					t.Fatal("Update must not be called once the claim has been stolen by another instance")
					return nil
				},
			},
			wantErr: true,
		},
		"ClaimClearedAbortsToo": {
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(nil),
				MockUpdate: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
					t.Fatal("Update must not be called once the claim has been cleared")
					return nil
				},
			},
			wantErr: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := NewManager(tc.client, cfg)
			m.now = func() time.Time { return fixedNow }

			err := m.Heartbeat(context.Background(), testWorkspace(""))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Heartbeat(...): err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestRelease(t *testing.T) {
	cfg := Config{HolderIdentity: "instance-1", Namespace: "provider-ns", TTL: 90 * time.Second}

	cases := map[string]struct {
		client  *test.MockClient
		wantErr bool
	}{
		"ClearsOwnClaim": {
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
					l.Spec.HolderIdentity = holderPtr("instance-1")
					l.Spec.RenewTime = microTime(time.Now())
					return nil
				}),
				MockUpdate: test.NewMockUpdateFn(nil, func(obj client.Object) error {
					l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
					if l.Spec.HolderIdentity != nil {
						t.Errorf("HolderIdentity = %q, want cleared (nil)", *l.Spec.HolderIdentity)
					}
					if l.Spec.RenewTime != nil {
						t.Error("RenewTime want cleared (nil)")
					}
					return nil
				}),
			},
		},
		"NoOpWhenNotHeldByUs": {
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(nil, func(obj client.Object) error {
					l := obj.(*coordinationv1.Lease) //nolint:forcetypeassert // test double, always a Lease
					l.Spec.HolderIdentity = holderPtr("instance-2")
					return nil
				}),
				MockUpdate: func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error {
					t.Fatal("Update must not be called releasing a claim this instance doesn't hold")
					return nil
				},
			},
		},
		"NoOpWhenLeaseAlreadyGone": {
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(kerrors.NewNotFound(coordinationv1.Resource("leases"), "uid-a")),
			},
		},
		"GetErrorPropagates": {
			client: &test.MockClient{
				MockGet: test.NewMockGetFn(errors.New("boom")),
			},
			wantErr: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := NewManager(tc.client, cfg)
			if err := m.Release(context.Background(), testWorkspace("")); (err != nil) != tc.wantErr {
				t.Fatalf("Release(...): err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
