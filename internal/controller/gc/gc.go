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
	"path/filepath"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/spf13/afero"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/upbound/provider-terraform/internal/workdir"
)

// Setup initializes and registers the garbage collectors with the manager.
//
// Two GC instances are created:
// - One for the main workspace directory containing workspace roots
// - One for /tmp directory containing temporary workspace files
//
// Each GC queries both cluster-scoped and namespaced workspaces to determine
// which directories can be safely deleted.
//
// The GC uses mgr.GetAPIReader() (uncached) instead of mgr.GetClient()
// (cached) to list workspaces. In sharded mode the manager's cache is
// filtered by shard label, so using the cached client would only return
// this shard's workspaces and the GC could delete directories belonging
// to other shards' workspaces.
func Setup(mgr ctrl.Manager, tfDir string, logger logging.Logger, shardName string) error {
	fs := afero.Afero{Fs: afero.NewOsFs()}

	gcOpts := []workdir.GarbageCollectorOption{
		workdir.WithFs(fs),
		workdir.WithLogger(logger),
	}
	if shardName != "" {
		gcOpts = append(gcOpts, workdir.WithShardName(shardName))
	}

	// GC for main workspace directory
	gcWorkspace := workdir.NewGarbageCollector(
		mgr.GetAPIReader(),
		tfDir,
		gcOpts...,
	)
	if err := mgr.Add(gcWorkspace); err != nil {
		return err
	}

	// GC for temporary workspace directory
	gcTmp := workdir.NewGarbageCollector(
		mgr.GetAPIReader(),
		filepath.Join("/tmp", tfDir),
		gcOpts...,
	)
	if err := mgr.Add(gcTmp); err != nil {
		return err
	}

	logger.Debug("Workspace garbage collectors initialized successfully", "shard-name", shardName)

	return nil
}
