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
	"time"

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
// interval controls how often each GC runs. A zero value keeps the
// workdir package default (one hour).
func Setup(mgr ctrl.Manager, tfDir string, logger logging.Logger, interval time.Duration) error {
	fs := afero.Afero{Fs: afero.NewOsFs()}

	opts := []workdir.GarbageCollectorOption{
		workdir.WithFs(fs),
		workdir.WithLogger(logger),
	}
	if interval > 0 {
		opts = append(opts, workdir.WithInterval(interval))
	}

	// GC for main workspace directory
	gcWorkspace := workdir.NewGarbageCollector(mgr.GetClient(), tfDir, opts...)
	if err := mgr.Add(gcWorkspace); err != nil {
		return err
	}

	// GC for temporary workspace directory
	gcTmp := workdir.NewGarbageCollector(mgr.GetClient(), filepath.Join("/tmp", tfDir), opts...)
	if err := mgr.Add(gcTmp); err != nil {
		return err
	}

	logger.Debug("Workspace garbage collectors initialized successfully", "interval", interval.String())

	return nil
}
