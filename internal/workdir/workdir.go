/*
Copyright 2021 The Crossplane Authors.

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

package workdir

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/spf13/afero"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1beta1 "github.com/upbound/provider-terraform/apis/cluster/v1beta1"
	namespacedv1beta1 "github.com/upbound/provider-terraform/apis/namespaced/v1beta1"
)

// Error strings.
const (
	errListWorkspaces = "cannot list workspaces"
	errFmtReadDir     = "cannot read directory %q"
)

// A GarbageCollector garbage collects the working directories of Terraform
// workspaces that no longer exist.
type GarbageCollector struct {
	kube      client.Client
	parentDir string
	fs        afero.Afero
	interval  time.Duration
	log       logging.Logger
}

// A GarbageCollectorOption configures a new GarbageCollector.
type GarbageCollectorOption func(*GarbageCollector)

// WithFs configures the afero filesystem implementation in which work dirs will
// be garbage collected. The default is the real operating system filesystem.
func WithFs(fs afero.Afero) GarbageCollectorOption {
	return func(gc *GarbageCollector) { gc.fs = fs }
}

// WithInterval configures how often garbage collection will run. The default
// interval is one hour.
func WithInterval(i time.Duration) GarbageCollectorOption {
	return func(gc *GarbageCollector) { gc.interval = i }
}

// WithLogger configures the logger that will be used. The default is a no-op
// logger never emits logs.
func WithLogger(l logging.Logger) GarbageCollectorOption {
	return func(gc *GarbageCollector) { gc.log = l }
}

// NewGarbageCollector returns a garbage collector that garbage collects the
// working directories of Terraform workspaces.
func NewGarbageCollector(c client.Client, parentDir string, o ...GarbageCollectorOption) *GarbageCollector {
	gc := &GarbageCollector{
		kube:      c,
		parentDir: parentDir,
		fs:        afero.Afero{Fs: afero.NewOsFs()},
		interval:  1 * time.Hour,
		log:       logging.NewNopLogger(),
	}

	for _, fn := range o {
		fn(gc)
	}

	return gc
}

// Run the garbage collector. Blocks until the supplied context is done.
func (gc *GarbageCollector) Run(ctx context.Context) {
	t := time.NewTicker(gc.interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := gc.collect(ctx); err != nil {
				gc.log.Info("Garbage collection failed", "error", err)
			}
		}
	}
}

// Start implements manager.Runnable interface.
// This allows the GarbageCollector to be managed by the controller-runtime manager.
func (gc *GarbageCollector) Start(ctx context.Context) error {
	gc.Run(ctx)
	return nil
}

func isUUID(u string) bool {
	_, err := uuid.Parse(u)
	return err == nil
}

func (gc *GarbageCollector) collect(ctx context.Context) error { //nolint:gocyclo // easier to follow as a unit
	gc.log.Debug("Running workspace garbage collection", "dir", gc.parentDir)
	exists := map[string]bool{}
	listedAny := false

	// List cluster-scoped workspaces
	// Note: CRD may not be available if CRD gating is enabled and CRD not installed
	clusterList := &clusterv1beta1.WorkspaceList{}
	if err := gc.kube.List(ctx, clusterList); err != nil {
		// Handle expected errors with CRD gating:
		// - 404 (NotFound): CRD not installed, no workspaces of this type exist (safe to continue)
		// - 403 (Forbidden): No RBAC permissions, workspaces might exist (abort GC)
		// - Other errors: Network/API issues, can't determine safely (abort GC)
		switch {
		case apierrors.IsNotFound(err):
			gc.log.Debug("Cluster-scoped Workspace CRD not installed, skipping")
		case apierrors.IsForbidden(err):
			gc.log.Info("No RBAC permissions to list cluster-scoped workspaces, aborting garbage collection")
			return err
		default:
			gc.log.Info("Failed to list cluster-scoped workspaces, aborting garbage collection")
			return err
		}
	} else {
		listedAny = true
		for _, ws := range clusterList.Items {
			exists[string(ws.GetUID())] = true
		}
	}

	// List namespaced workspaces
	// Note: CRD may not be available if CRD gating is enabled and CRD not installed
	namespacedList := &namespacedv1beta1.WorkspaceList{}
	if err := gc.kube.List(ctx, namespacedList); err != nil {
		// Handle expected errors with CRD gating:
		// - 404 (NotFound): CRD not installed, no workspaces of this type exist (safe to continue)
		// - 403 (Forbidden): No RBAC permissions, workspaces might exist (abort GC)
		// - Other errors: Network/API issues, can't determine safely (abort GC)
		switch {
		case apierrors.IsNotFound(err):
			gc.log.Debug("Namespaced Workspace CRD not installed, skipping")
		case apierrors.IsForbidden(err):
			gc.log.Debug("No RBAC permissions to list namespaced workspaces, aborting garbage collection")
			return err
		default:
			gc.log.Debug("Failed to list namespaced workspaces, aborting garbage collection")
			return err
		}
	} else {
		listedAny = true
		for _, ws := range namespacedList.Items {
			exists[string(ws.GetUID())] = true
		}
	}

	// If we couldn't list any workspace types, skip cleanup to avoid
	// accidentally deleting active workspace directories
	if !listedAny {
		gc.log.Debug("No Workspace MRs available, skipping garbage collection")
		return nil
	}

	fis, err := gc.fs.ReadDir(gc.parentDir)
	if err != nil {
		return errors.Wrapf(err, errFmtReadDir, gc.parentDir)
	}

	failed := make([]string, 0)
	for _, fi := range fis {
		if !fi.IsDir() || !isUUID(fi.Name()) {
			continue
		}
		if exists[fi.Name()] {
			continue
		}
		path := filepath.Join(gc.parentDir, fi.Name())
		if err := gc.fs.RemoveAll(path); err != nil {
			failed = append(failed, path)
		}
	}

	if len(failed) > 0 {
		return errors.Errorf("could not delete directories: %v", strings.Join(failed, ", "))
	}

	return nil
}
