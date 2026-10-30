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

// Package handover keeps a Workspace reconcilable when its assignment label is
// flipped to another instance while a terraform run is still in flight.
//
// Sharding a Workspace onto another instance mid-apply does two things at once.
// It bumps the Workspace's resourceVersion, so the copy the running reconcile
// has been holding since before the relabel is stale and its next write
// conflicts. And it evicts the Workspace from the outgoing instance's cache,
// because --watch-label-selector makes that cache label-filtered and the API
// server delivers "was matching, now isn't" as a DELETED watch event.
//
// Between them those two effects strand the Workspace. The reconciler stamps
// crossplane.io/external-create-pending before calling Create, and records
// external-create-succeeded once Create returns. If that second write cannot
// land, creation looks incomplete forever, and every instance -- including the
// one that just inherited the Workspace -- refuses to reconcile it at all:
// crossplane-runtime bails on ExternalCreateIncomplete before it ever calls
// Observe or Create, and does not requeue.
//
// ReconcilerOptions repairs the write. It deliberately leaves the refusal alone.
//
// # Why the create-pending gate is left in place
//
// managed.WithDeterministicExternalName(true) would make the reconciler proceed
// through an undeterminable create result rather than refuse. It looks
// applicable here, because a Workspace's external name really is deterministic:
// it is the Workspace's own name, assigned by the default NameAsExternalName
// initializer and only ever read by Create.
//
// That is the wrong reading of the option. What it asserts is that a create
// whose result was never recorded is safe to repeat, because Observe can still
// find whatever that create made. For a Workspace, Observe answers
// ResourceExists from `terraform state list` and `terraform output` -- so it can
// only find what the Terraform state records. A run killed between provisioning
// a resource and that resource reaching persisted state leaves nothing for
// Observe to find, and the next apply provisions it a second time. Terraform
// workspace names are deterministic; the cloud resources a module creates are
// not.
//
// Whether that risk is real depends on the backend each module declares, which
// this provider cannot know. So the call belongs to whoever owns the Workspace,
// made against the actual state:
//
//	kubectl annotate workspace <name> crossplane.io/external-create-pending-
//
// That only arises when the owning instance dies mid-apply and records no
// outcome at all -- a crash, an OOM kill, an eviction, a --timeout kill. It is
// upstream behaviour rather than anything sharding introduced, and an instance
// that merely loses a Workspace to another shard still records its result.
package handover

import (
	"context"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReconcilerOptions returns the managed reconciler options that let a Workspace
// survive a mid-apply relabel. c is the manager's client and reader reads
// straight from the API server -- manager.GetAPIReader().
func ReconcilerOptions(c client.Client, reader client.Reader) []managed.ReconcilerOption {
	return []managed.ReconcilerOption{
		// Persist critical annotations through a client whose reads bypass the
		// cache. RetryingCriticalAnnotationUpdater recovers from a stale
		// resourceVersion by re-Getting the object, re-applying the
		// annotations, and retrying the write -- which is exactly the recovery
		// a relabel needs, and exactly the read a label-filtered cache can no
		// longer serve. Reading from the API server instead lets the outgoing
		// instance record its create result on a Workspace it no longer owns,
		// without clobbering the relabel that handed it over.
		managed.WithCriticalAnnotationUpdater(
			managed.NewRetryingCriticalAnnotationUpdater(Uncached(c, reader)),
		),
	}
}

// Uncached returns c with its reads redirected to reader. Writes still go
// through c: a client.Reader cannot write, and writes never went through the
// cache to begin with.
func Uncached(c client.Client, reader client.Reader) client.Client {
	return &uncachedReads{Client: c, reader: reader}
}

// uncachedReads is a client.Client whose Get and List bypass the cache.
type uncachedReads struct {
	client.Client
	reader client.Reader
}

func (c *uncachedReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.reader.Get(ctx, key, obj, opts...)
}

func (c *uncachedReads) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}
