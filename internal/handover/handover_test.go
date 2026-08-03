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

package handover

import (
	"context"
	"reflect"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/test"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/upbound/provider-terraform/apis/cluster/v1beta1"
)

// shardLabel is the assignment label documented for --watch-label-selector.
const shardLabel = "sharding.example.com/shard"

var (
	errCacheServedRead = errors.New("read served by the cache")

	workspaceResource = schema.GroupResource{Group: v1beta1.Group, Resource: "workspaces"}
)

// evictedCache serves reads the way a label-filtered informer does once an
// object stops matching this instance's selector: it no longer has it.
func evictedCache() *test.MockClient {
	return &test.MockClient{
		MockGet:  func(_ context.Context, _ client.ObjectKey, _ client.Object) error { return errCacheServedRead },
		MockList: func(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error { return errCacheServedRead },
	}
}

func TestUncachedReadsBypassTheCache(t *testing.T) {
	var gotGet, gotList bool
	reader := &test.MockClient{
		MockGet:  func(_ context.Context, _ client.ObjectKey, _ client.Object) error { gotGet = true; return nil },
		MockList: func(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error { gotList = true; return nil },
	}

	c := Uncached(evictedCache(), reader)

	if err := c.Get(context.Background(), client.ObjectKey{Name: "ws"}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("Get(...): unexpected error: %v", err)
	}
	if !gotGet {
		t.Error("Get(...) was served by the wrapped client, want the reader")
	}

	if err := c.List(context.Background(), &corev1.ConfigMapList{}); err != nil {
		t.Errorf("List(...): unexpected error: %v", err)
	}
	if !gotList {
		t.Error("List(...) was served by the wrapped client, want the reader")
	}
}

func TestUncachedWritesUseTheWrappedClient(t *testing.T) {
	var gotCreate, gotUpdate, gotDelete bool
	wrapped := evictedCache()
	wrapped.MockCreate = func(_ context.Context, _ client.Object, _ ...client.CreateOption) error { gotCreate = true; return nil }
	wrapped.MockUpdate = func(_ context.Context, _ client.Object, _ ...client.UpdateOption) error { gotUpdate = true; return nil }
	wrapped.MockDelete = func(_ context.Context, _ client.Object, _ ...client.DeleteOption) error { gotDelete = true; return nil }

	reader := &test.MockClient{
		MockGet: func(_ context.Context, _ client.ObjectKey, _ client.Object) error {
			t.Error("a write was routed to the reader")
			return nil
		},
	}

	c := Uncached(wrapped, reader)
	ctx := context.Background()

	if err := c.Create(ctx, &corev1.ConfigMap{}); err != nil {
		t.Errorf("Create(...): unexpected error: %v", err)
	}
	if err := c.Update(ctx, &corev1.ConfigMap{}); err != nil {
		t.Errorf("Update(...): unexpected error: %v", err)
	}
	if err := c.Delete(ctx, &corev1.ConfigMap{}); err != nil {
		t.Errorf("Delete(...): unexpected error: %v", err)
	}

	if !gotCreate || !gotUpdate || !gotDelete {
		t.Errorf("writes did not reach the wrapped client: create=%v update=%v delete=%v", gotCreate, gotUpdate, gotDelete)
	}
}

// TestReconcilerOptionsSetsDeterministicExternalName pins the first half of the
// fix: without it the reconciler refuses to act on any Workspace whose create
// result it cannot determine, which is every Workspace relabelled mid-apply.
func TestReconcilerOptionsSetsDeterministicExternalName(t *testing.T) {
	r := applyOptions(&test.MockClient{}, &test.MockClient{})

	f := reflect.ValueOf(r).Elem().FieldByName("deterministicExternalName")
	if !f.IsValid() || f.Kind() != reflect.Bool {
		t.Fatal("managed.Reconciler no longer has a bool deterministicExternalName field: check whether " +
			"WithDeterministicExternalName still exists after the crossplane-runtime bump, and update this test")
	}
	if !f.Bool() {
		t.Error("ReconcilerOptions() did not set a deterministic external name; a Workspace relabelled mid-apply will wedge")
	}
}

// TestCriticalAnnotationsSurviveMidApplyRelabel pins the second half, and is
// the regression test for the shard-handover deadlock.
//
// A Workspace is created under shard-1. The reconciler stamps
// external-create-pending, persists it, and starts a long terraform apply.
// Mid-apply the Workspace is relabelled onto shard-2, which does two things at
// once: it bumps the resourceVersion, so shard-1's in-flight copy goes stale
// and its next write conflicts; and it evicts the Workspace from shard-1's
// label-filtered cache, because the API server delivers "was matching, now
// isn't" as a DELETED watch event.
//
// When the apply finishes shard-1 has to record external-create-succeeded.
// Recovering from the conflict through the cache cannot work -- the object is
// no longer there -- so the annotation is lost and creation looks permanently
// incomplete, which makes shard-2 refuse to reconcile the Workspace ever again:
// crossplane-runtime bails on ExternalCreateIncomplete before it ever calls
// Observe or Create, and does not requeue. Recovering through the API server
// lets the write land, and the handover completes.
//
// The updater is type-agnostic, so one concrete type is enough. It uses the
// real cluster-scoped Workspace so the assertions are the same predicate that
// gates the reconciler in production.
func TestCriticalAnnotationsSurviveMidApplyRelabel(t *testing.T) {
	pending := time.Date(2026, 7, 30, 17, 24, 0, 0, time.UTC)
	succeeded := pending.Add(10 * time.Minute)

	// relabelled is the Workspace as it exists on the API server once the
	// shard label has been flipped: shard-2, resourceVersion 2, create-pending.
	relabelled := func() *fakeAPI {
		ws := &v1beta1.Workspace{ObjectMeta: metav1.ObjectMeta{
			Name:            "ws-s1-long-01-test",
			ResourceVersion: "2",
			Labels:          map[string]string{shardLabel: "2"},
		}}
		meta.SetExternalCreatePending(ws, pending)
		return &fakeAPI{ws: ws}
	}

	// inflight is the copy shard-1's reconcile has held since before the
	// relabel -- resourceVersion 1, shard-1 -- with the succeeded annotation
	// just stamped on it by the reconciler.
	inflight := func() *v1beta1.Workspace {
		ws := &v1beta1.Workspace{ObjectMeta: metav1.ObjectMeta{
			Name:            "ws-s1-long-01-test",
			ResourceVersion: "1",
			Labels:          map[string]string{shardLabel: "1"},
		}}
		meta.SetExternalCreatePending(ws, pending)
		meta.SetExternalCreateSucceeded(ws, succeeded)
		return ws
	}

	// shardOneCache is the outgoing instance's label-filtered cache: it can
	// still write, but it can no longer read the relabelled Workspace.
	shardOneCache := func(a *fakeAPI) *test.MockClient {
		return &test.MockClient{
			MockGet: func(_ context.Context, key client.ObjectKey, _ client.Object) error {
				return kerrors.NewNotFound(workspaceResource, key.Name)
			},
			MockUpdate: func(_ context.Context, obj client.Object, _ ...client.UpdateOption) error {
				return a.update(obj)
			},
		}
	}

	cases := map[string]struct {
		reason         string
		updater        func(t *testing.T, a *fakeAPI) managed.CriticalAnnotationUpdater
		wantErr        bool
		wantIncomplete bool
	}{
		"CachedReadsWedgeTheWorkspace": {
			reason: "Recovering from the relabel's conflict via the label-filtered cache cannot work, and leaves creation permanently incomplete.",
			updater: func(_ *testing.T, a *fakeAPI) managed.CriticalAnnotationUpdater {
				return managed.NewRetryingCriticalAnnotationUpdater(shardOneCache(a))
			},
			wantErr:        true,
			wantIncomplete: true,
		},
		"ReconcilerOptionsCompleteTheHandover": {
			reason: "The updater ReconcilerOptions installs recovers via the API server, so the outgoing instance records its create result and releases the Workspace to its new shard.",
			updater: func(t *testing.T, a *fakeAPI) managed.CriticalAnnotationUpdater {
				reader := &test.MockClient{
					MockGet: func(_ context.Context, _ client.ObjectKey, obj client.Object) error { return a.get(obj) },
				}
				return criticalAnnotationUpdater(t, applyOptions(shardOneCache(a), reader))
			},
			wantErr:        false,
			wantIncomplete: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := relabelled()

			err := tc.updater(t, a).UpdateCriticalAnnotations(context.Background(), inflight())

			if tc.wantErr && err == nil {
				t.Fatalf("UpdateCriticalAnnotations(...): want error, got none\n%s", tc.reason)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("UpdateCriticalAnnotations(...): unexpected error: %v\n%s", err, tc.reason)
			}

			if got := meta.ExternalCreateIncomplete(a.ws); got != tc.wantIncomplete {
				t.Errorf("ExternalCreateIncomplete(...): want %v, got %v\n%s", tc.wantIncomplete, got, tc.reason)
			}

			if tc.wantIncomplete {
				return
			}

			// The outgoing instance writes the create result without stomping
			// the relabel that handed the Workspace over: it re-read the
			// object, then re-applied only its annotations.
			if got := a.ws.GetLabels()[shardLabel]; got != "2" {
				t.Errorf("shard label: want %q, got %q -- the outgoing instance reverted the handover", "2", got)
			}
		})
	}
}

// applyOptions builds the Reconciler that ReconcilerOptions would configure.
// managed.Reconciler's fields are unexported, so a zero value plus the options
// is the only way to observe them without a live manager.
func applyOptions(c client.Client, reader client.Reader) *managed.Reconciler {
	r := &managed.Reconciler{}
	for _, opt := range ReconcilerOptions(c, reader) {
		opt(r)
	}
	return r
}

func criticalAnnotationUpdater(t *testing.T, r *managed.Reconciler) managed.CriticalAnnotationUpdater {
	t.Helper()

	f := reflect.ValueOf(r).Elem().FieldByName("managed")
	if f.IsValid() {
		f = f.FieldByName("CriticalAnnotationUpdater")
	}
	if !f.IsValid() {
		t.Fatal("managed.Reconciler no longer holds a CriticalAnnotationUpdater where this test expects it: " +
			"check whether WithCriticalAnnotationUpdater still exists after the crossplane-runtime bump, and update this test")
	}

	// Reading an unexported field's interface value needs NewAt; the vet-safe
	// form is a direct unsafe.Pointer(UnsafeAddr()) conversion.
	u, ok := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(managed.CriticalAnnotationUpdater)
	if !ok || u == nil {
		t.Fatal("ReconcilerOptions() did not set a CriticalAnnotationUpdater")
	}
	return u
}

// fakeAPI is a minimal stand-in for the API server's optimistic concurrency: an
// Update carrying a stale resourceVersion is rejected with a conflict, and a
// successful one bumps the stored resourceVersion and is written back into the
// caller's object, as a real client does.
type fakeAPI struct {
	ws *v1beta1.Workspace
}

func (a *fakeAPI) get(obj client.Object) error {
	ws, ok := obj.(*v1beta1.Workspace)
	if !ok {
		return errors.Errorf("unexpected object type %T", obj)
	}
	a.ws.DeepCopyInto(ws)
	return nil
}

func (a *fakeAPI) update(obj client.Object) error {
	ws, ok := obj.(*v1beta1.Workspace)
	if !ok {
		return errors.Errorf("unexpected object type %T", obj)
	}
	if ws.GetResourceVersion() != a.ws.GetResourceVersion() {
		return kerrors.NewConflict(workspaceResource, ws.GetName(),
			errors.New("the object has been modified; please apply your changes to the latest version and try again"))
	}

	rv, err := strconv.Atoi(a.ws.GetResourceVersion())
	if err != nil {
		return err
	}
	stored := ws.DeepCopy()
	stored.SetResourceVersion(strconv.Itoa(rv + 1))
	a.ws = stored
	stored.DeepCopyInto(ws)
	return nil
}
