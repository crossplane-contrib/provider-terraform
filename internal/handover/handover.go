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
// Between them those two effects can strand a Workspace permanently. The
// reconciler stamps crossplane.io/external-create-pending before calling
// Create, and records external-create-succeeded once Create returns. If the
// second write cannot land, creation looks incomplete forever, and every
// instance -- including the one that just inherited the Workspace -- refuses to
// reconcile it at all: crossplane-runtime bails on ExternalCreateIncomplete
// before it ever calls Observe or Create, and does not requeue. Only a human
// removing the annotation clears it.
//
// ReconcilerOptions closes both halves of that trap.
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
		// A Workspace's external name is its own name: assigned by the default
		// NameAsExternalName initializer, and only ever read by Create. Saying
		// so lets the reconciler proceed when it cannot determine a previous
		// Create's result instead of refusing to touch the Workspace until a
		// human intervenes. The refusal exists to avoid leaking a resource
		// whose non-deterministic name was never recorded; with no such name to
		// lose it only ever wedges the Workspace -- after a mid-apply relabel,
		// or a pod restart mid-apply.
		managed.WithDeterministicExternalName(true),

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
