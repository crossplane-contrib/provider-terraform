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
	"time"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrBackoff is returned by Guard when a foreign, fresh claim prevented a
// run (R3). Callers should surface this as a quiet, expected condition --
// not a Terraform failure -- and let the normal reconcile requeue retry.
var ErrBackoff = errors.New("workspace deferred: ownership claim held by another instance")

// GuardFunc is work Guard performs on ws's behalf.
type GuardFunc func(ctx context.Context) error

// Guard runs fn only while this instance holds ws's ownership claim (R1) --
// the single seam both the cluster-scoped and namespaced Workspace
// reconcilers call before a state-mutating Terraform run (Apply or
// Destroy). It implements design doc §8:
//
//   - R2 (empty/own claim): acquire, then run fn immediately.
//   - R3 (foreign, fresh claim): fn is not called; Guard returns ErrBackoff
//     so the caller requeues with backoff instead of running.
//   - R4 (foreign, stale claim): the claim is stolen and fn runs, exactly as
//     for R2. Guard deliberately does not clear any Terraform state lock the
//     presumed-dead owner left behind -- see below.
//   - R5: while fn runs, the claim is heartbeated every cfg.HeartbeatInterval.
//   - R6: the claim is released when fn returns, success or failure.
//   - R7 (a label flip never interrupts a run): Guard has no part in this --
//     it holds only for the fn call, which the caller's existing synchronous
//     reconcile + exec.CommandContext binding already protects.
//
// A stolen claim never implies the state lock may be broken. The lock exists
// because an apply was interrupted, so the state behind it is unknown:
// clearing it would let fn apply over partially-written state and duplicate
// real infrastructure. A stale claim is also not proof the old owner died --
// it only proves its heartbeats stopped. So fn runs, Terraform fails to
// acquire the lock, and the error surfaces for a human to resolve. Acquire
// logs the takeover, which is what lets that human tell an orphaned lock
// apart from one a live instance is still holding.
//
// If cfg.Enabled is false, Guard is a transparent pass-through to fn --
// this is the parity guarantee for the (default) disabled case.
func Guard(ctx context.Context, mgr *Manager, cfg Config, ws client.Object, ownerGVK schema.GroupVersionKind, fn GuardFunc) error {
	if !cfg.Enabled {
		return fn(ctx)
	}

	outcome, err := mgr.Acquire(ctx, ws, ownerGVK)
	if err != nil {
		return err
	}
	if outcome == Backoff {
		return ErrBackoff
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go heartbeat(ctx, mgr, ws, cfg.HeartbeatInterval, stop, done)

	runErr := fn(ctx)

	close(stop)
	<-done

	if relErr := mgr.Release(ctx, ws); relErr != nil && runErr == nil {
		return relErr
	}
	return runErr
}

// heartbeat renews ws's claim every interval until stop is closed, then
// closes done. Heartbeat failures are not fatal to the in-flight run --
// R7 relies on the run completing regardless (the zombie-window residual
// risk noted in design §8 is bounded by the state lock, not by aborting
// heartbeats mid-run).
func heartbeat(ctx context.Context, mgr *Manager, ws client.Object, interval time.Duration, stop, done chan struct{}) {
	defer close(done)

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-t.C:
			_ = mgr.Heartbeat(ctx, ws)
		case <-stop:
			return
		}
	}
}
