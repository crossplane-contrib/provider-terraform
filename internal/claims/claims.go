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

// Package claims implements the optional per-Workspace ownership-claim
// handover protocol (design doc §8, TD-6). A claim is stored in a dedicated
// coordination.k8s.io/v1 Lease named after the Workspace's UID, separate
// from the Workspace's assignment label (the label says who *should* own a
// Workspace; the claim says who *actually* runs terraform on it).
package claims

import (
	"context"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/pkg/errors"
	coordinationv1 "k8s.io/api/coordination/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Outcome describes the result of an Acquire call (design §8 R2-R4).
type Outcome int

const (
	// Acquired means the caller now holds the claim and may run terraform
	// (the claim was empty or already held by this instance).
	Acquired Outcome = iota
	// Backoff means a foreign, fresh claim is held by another instance (R3),
	// or this instance lost a concurrent acquire race (R2); the caller must
	// not run and should requeue.
	Backoff
	// Stolen means a foreign, stale claim was overwritten (R4). The caller
	// now holds the claim and may run terraform. It must not clear any state
	// lock the previous owner left behind: a stale claim proves only that
	// heartbeats stopped, not that the apply did, and the state under a lock
	// left by an interrupted apply is unknown. Acquire logs every takeover --
	// see msgClaimStolen.
	Stolen
)

// Errors returned by Manager methods.
const (
	errGetLease    = "cannot get ownership claim lease"
	errCreateLease = "cannot create ownership claim lease"
	errUpdateLease = "cannot update ownership claim lease"
	errClearLease  = "cannot clear ownership claim lease"
	errNotHolder   = "cannot heartbeat: this instance no longer holds the claim"
)

const msgClaimStolen = "Took over a stale ownership claim from a presumed-dead owner"

// Config configures a Manager (design §7.1 flags).
type Config struct {
	// Enabled turns on ownership-claim enforcement
	// (--enable-ownership-claims). When false, callers should bypass the
	// Manager entirely; today's bare-relabel behavior is already correct
	// per design §5.4.
	Enabled bool

	// TTL is the staleness threshold: a claim not renewed within TTL is
	// presumed abandoned by a dead owner (--ownership-claim-ttl, default
	// 90s -- three missed heartbeats at the default interval).
	TTL time.Duration

	// HeartbeatInterval is how often a held claim's renewTime is refreshed
	// while a run executes (--ownership-heartbeat-interval, default 30s).
	HeartbeatInterval time.Duration

	// HolderIdentity uniquely identifies this instance (e.g. its pod name).
	HolderIdentity string

	// Logger receives claim-takeover records (msgClaimStolen). It may be left
	// nil, in which case NewManager substitutes a no-op logger.
	Logger logging.Logger

	// Namespace holds claim Leases for cluster-scoped Workspaces, which have
	// no namespace of their own. Namespaced Workspaces' claims live in the
	// Workspace's own namespace instead, so the Lease's owner reference
	// stays same-namespace and valid -- Kubernetes silently drops (and
	// never garbage-collects on) a cross-namespace owner reference.
	Namespace string
}

// Manager guards Terraform runs with a per-Workspace ownership claim stored
// in a coordination.k8s.io/v1 Lease (design §8).
type Manager struct {
	kube client.Client
	cfg  Config
	now  func() time.Time
}

// NewManager returns a claim Manager backed by kube.
func NewManager(kube client.Client, cfg Config) *Manager {
	if cfg.Logger == nil {
		cfg.Logger = logging.NewNopLogger()
	}
	return &Manager{kube: kube, cfg: cfg, now: time.Now}
}

// claimKey returns the claim Lease's namespace and name for ws.
func claimKey(cfg Config, ws client.Object) client.ObjectKey {
	ns := ws.GetNamespace()
	if ns == "" {
		ns = cfg.Namespace
	}
	return client.ObjectKey{Namespace: ns, Name: string(ws.GetUID())}
}

// Acquire attempts to take the ownership claim for ws (R2-R4), creating its
// Lease if it doesn't exist yet. ownerGVK identifies ws's concrete type
// (cluster-scoped or namespaced Workspace) so the new Lease can carry a
// correct owner reference; Manager itself has no dependency on either
// Workspace API package.
func (m *Manager) Acquire(ctx context.Context, ws client.Object, ownerGVK schema.GroupVersionKind) (Outcome, error) {
	key := claimKey(m.cfg, ws)

	lease := &coordinationv1.Lease{}
	err := m.kube.Get(ctx, key, lease)
	if kerrors.IsNotFound(err) {
		return m.create(ctx, key, ws, ownerGVK)
	}
	if err != nil {
		return Backoff, errors.Wrap(err, errGetLease)
	}

	holder := ""
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}
	foreign := holder != "" && holder != m.cfg.HolderIdentity

	if foreign && m.isFresh(lease) {
		return Backoff, nil // R3
	}

	// How long the previous owner had been silent, read before stamp
	// overwrites its heartbeat. A claim with no renewTime at all reads as
	// never heartbeated rather than as zero staleness.
	staleFor := "never heartbeated"
	if lease.Spec.RenewTime != nil {
		staleFor = m.now().Sub(lease.Spec.RenewTime.Time).String()
	}

	m.stamp(lease)
	if err := m.kube.Update(ctx, lease); err != nil {
		if kerrors.IsConflict(err) {
			return Backoff, nil // R2: another racer's write won the resourceVersion race
		}
		return Backoff, errors.Wrap(err, errUpdateLease)
	}
	// Signifies that the lease is acquired from another owner with a stale
	// claim. Log it: this is the only record that the Workspace changed hands
	// because its previous owner stopped heartbeating.
	if foreign {
		m.cfg.Logger.Info(msgClaimStolen,
			"workspace", ws.GetName(),
			"claim-lease", key.String(),
			"previous-holder", holder,
			"holder", m.cfg.HolderIdentity,
			"stale-for", staleFor)
		return Stolen, nil // R4
	}
	return Acquired, nil // R2: claim was empty or already ours
}

func (m *Manager) create(ctx context.Context, key client.ObjectKey, ws client.Object, ownerGVK schema.GroupVersionKind) (Outcome, error) {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       key.Namespace,
			Name:            key.Name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ws, ownerGVK)},
		},
	}
	m.stamp(lease)
	if err := m.kube.Create(ctx, lease); err != nil {
		if kerrors.IsAlreadyExists(err) {
			return Backoff, nil // R2: another racer created it first
		}
		return Backoff, errors.Wrap(err, errCreateLease)
	}
	return Acquired, nil
}

// isFresh reports whether lease's last heartbeat is within TTL.
func (m *Manager) isFresh(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil {
		return false
	}
	return m.now().Sub(lease.Spec.RenewTime.Time) < m.cfg.TTL
}

// stamp sets lease's holder, renew time, and duration to this instance's
// current claim.
func (m *Manager) stamp(lease *coordinationv1.Lease) {
	now := metav1.NewMicroTime(m.now())
	holder := m.cfg.HolderIdentity
	dur := int32(m.cfg.TTL / time.Second)
	lease.Spec.HolderIdentity = &holder
	lease.Spec.RenewTime = &now
	lease.Spec.LeaseDurationSeconds = &dur
}

// Heartbeat renews the claim's renewTime while a run is executing (R5). It
// returns an error if this instance no longer holds the claim -- the
// zombie-window case (design §8 residual risk: a paused-then-resumed owner
// may briefly believe it still holds the claim; its next heartbeat write
// must fail so it can abort).
func (m *Manager) Heartbeat(ctx context.Context, ws client.Object) error {
	key := claimKey(m.cfg, ws)
	lease := &coordinationv1.Lease{}
	if err := m.kube.Get(ctx, key, lease); err != nil {
		return errors.Wrap(err, errGetLease)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != m.cfg.HolderIdentity {
		return errors.New(errNotHolder)
	}
	m.stamp(lease)
	return errors.Wrap(m.kube.Update(ctx, lease), errUpdateLease)
}

// Release clears the claim when a reconcile ends, success or failure (R6).
// It is a no-op if the claim was already cleared, already stolen by another
// instance, or its Lease no longer exists.
func (m *Manager) Release(ctx context.Context, ws client.Object) error {
	key := claimKey(m.cfg, ws)
	lease := &coordinationv1.Lease{}
	if err := m.kube.Get(ctx, key, lease); err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return errors.Wrap(err, errGetLease)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != m.cfg.HolderIdentity {
		return nil
	}
	lease.Spec.HolderIdentity = nil
	lease.Spec.RenewTime = nil
	return errors.Wrap(m.kube.Update(ctx, lease), errClearLease)
}
