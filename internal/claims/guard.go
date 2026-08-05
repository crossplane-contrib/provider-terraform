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
//   - R4 (foreign, stale claim): the claim is stolen, unlock runs first to
//     clear any state lock left by the presumed-dead owner, then fn runs.
//   - R5: while fn runs, the claim is heartbeated every cfg.HeartbeatInterval.
//   - R6: the claim is released when fn returns, success or failure.
//   - R7 (a label flip never interrupts a run): Guard has no part in this --
//     it holds only for the fn call, which the caller's existing synchronous
//     reconcile + exec.CommandContext binding already protects.
//
// If cfg.Enabled is false, Guard is a transparent pass-through to fn --
// this is the parity guarantee for the (default) disabled case.
func Guard(ctx context.Context, mgr *Manager, cfg Config, ws client.Object, ownerGVK schema.GroupVersionKind, unlock, fn GuardFunc) error {
	if !cfg.Enabled {
		return fn(ctx)
	}

	outcome, err := mgr.Acquire(ctx, ws, ownerGVK)
	if err != nil {
		return err
	}

	switch outcome {
	case Backoff:
		return ErrBackoff
	case Stolen:
		if err := unlock(ctx); err != nil {
			return err
		}
	case Acquired:
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
