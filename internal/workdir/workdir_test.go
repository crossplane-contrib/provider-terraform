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
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/pkg/errors"
	"github.com/spf13/afero"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/test"

	clusterv1beta1 "github.com/upbound/provider-terraform/apis/cluster/v1beta1"
	namespacedv1beta1 "github.com/upbound/provider-terraform/apis/namespaced/v1beta1"
)

func withDirs(fs afero.Afero, dir ...string) afero.Afero {
	for _, d := range dir {
		_ = fs.Mkdir(d, os.ModePerm)
	}
	return fs
}

func getDirs(fs afero.Afero, parentDir string) []string {
	dirs := make([]string, 0)
	fis, _ := fs.ReadDir(parentDir)
	for _, fi := range fis {
		if !fi.IsDir() {
			continue
		}
		dirs = append(dirs, fi.Name())
	}
	return dirs
}

func TestCollect(t *testing.T) {
	parentDir := "/test"

	type fields struct {
		kube       client.Client
		parentdDir string
		fs         afero.Afero
	}
	type args struct {
		ctx context.Context
	}
	type want struct {
		dirs []string
		err  error
	}
	cases := map[string]struct {
		reason string
		fields fields
		args   args
		want   want
	}{
		"ErrNoParentDir": {
			reason: "Garbage collection should fail when the parent directory does not exist.",
			fields: fields{
				kube:       &test.MockClient{MockList: test.NewMockListFn(nil)},
				parentdDir: parentDir,
				fs:         afero.Afero{Fs: afero.NewMemMapFs()},
			},
			want: want{
				err: errors.Wrapf(errors.Errorf("open %s: file does not exist", parentDir), errFmtReadDir, parentDir),
			},
		},
		"NoOp": {
			reason: "Garbage collection should succeed when there are no workspaces or workdirs.",
			fields: fields{
				kube: &test.MockClient{MockList: test.NewMockListFn(nil, func(obj client.ObjectList) error {
					// Return empty lists for both workspace types
					return nil
				})},
				parentdDir: parentDir,
				fs:         withDirs(afero.Afero{Fs: afero.NewMemMapFs()}, parentDir),
			},
			want: want{
				err: nil,
			},
		},
		"Success": {
			reason: "Workdirs belonging to workspaces that no longer exist should be successfully garbage collected.",
			fields: fields{
				kube: &test.MockClient{MockList: test.NewMockListFn(nil, func(obj client.ObjectList) error {
					// Handle both cluster and namespaced workspace lists
					switch v := obj.(type) {
					case *clusterv1beta1.WorkspaceList:
						*v = clusterv1beta1.WorkspaceList{Items: []clusterv1beta1.Workspace{
							{ObjectMeta: metav1.ObjectMeta{UID: types.UID("8371dd9e-dd3f-4a42-bd8c-340c4744f6de")}},
						}}
					case *namespacedv1beta1.WorkspaceList:
						*v = namespacedv1beta1.WorkspaceList{Items: []namespacedv1beta1.Workspace{
							{ObjectMeta: metav1.ObjectMeta{UID: types.UID("ebaac629-43a3-4b39-8138-d7ac19cafe11")}},
						}}
					}
					return nil
				})},
				parentdDir: parentDir,
				fs: withDirs(afero.Afero{Fs: afero.NewMemMapFs()},
					parentDir,
					filepath.Join(parentDir, "8371dd9e-dd3f-4a42-bd8c-340c4744f6de"),
					filepath.Join(parentDir, "ebaac629-43a3-4b39-8138-d7ac19cafe11"),
					filepath.Join(parentDir, "0d177133-1a2f-4ce2-93d2-f8212d3344e7"),
					filepath.Join(parentDir, "helm"),
					filepath.Join(parentDir, "registry.terraform.io"),
				),
			},
			want: want{
				dirs: []string{"8371dd9e-dd3f-4a42-bd8c-340c4744f6de", "ebaac629-43a3-4b39-8138-d7ac19cafe11", "helm", "registry.terraform.io"},
			},
		},
		"ClusterCRDNotFound": {
			reason: "GC should continue when cluster CRD not found (404), deleting orphaned dirs but protecting namespaced workspaces.",
			fields: fields{
				kube: &test.MockClient{MockList: test.NewMockListFn(nil, func(obj client.ObjectList) error {
					switch v := obj.(type) {
					case *clusterv1beta1.WorkspaceList:
						// Return 404 for cluster workspaces
						return apierrors.NewNotFound(schema.GroupResource{Group: "tf.upbound.io", Resource: "workspaces"}, "")
					case *namespacedv1beta1.WorkspaceList:
						*v = namespacedv1beta1.WorkspaceList{Items: []namespacedv1beta1.Workspace{
							{ObjectMeta: metav1.ObjectMeta{UID: types.UID("aaaa0000-0000-0000-0000-000000000001")}},
						}}
					}
					return nil
				})},
				parentdDir: parentDir,
				fs: withDirs(afero.Afero{Fs: afero.NewMemMapFs()},
					parentDir,
					filepath.Join(parentDir, "aaaa0000-0000-0000-0000-000000000001"),
					filepath.Join(parentDir, "bbbb0000-0000-0000-0000-000000000002"),
				),
			},
			want: want{
				dirs: []string{"aaaa0000-0000-0000-0000-000000000001"},
			},
		},
		"NamespacedCRDNotFound": {
			reason: "GC should continue when namespaced CRD not found (404), deleting orphaned dirs but protecting cluster workspaces.",
			fields: fields{
				kube: &test.MockClient{MockList: test.NewMockListFn(nil, func(obj client.ObjectList) error {
					switch v := obj.(type) {
					case *clusterv1beta1.WorkspaceList:
						*v = clusterv1beta1.WorkspaceList{Items: []clusterv1beta1.Workspace{
							{ObjectMeta: metav1.ObjectMeta{UID: types.UID("cccc0000-0000-0000-0000-000000000003")}},
						}}
					case *namespacedv1beta1.WorkspaceList:
						// Return 404 for namespaced workspaces
						return apierrors.NewNotFound(schema.GroupResource{Group: "tf.upbound.io", Resource: "workspaces"}, "")
					}
					return nil
				})},
				parentdDir: parentDir,
				fs: withDirs(afero.Afero{Fs: afero.NewMemMapFs()},
					parentDir,
					filepath.Join(parentDir, "cccc0000-0000-0000-0000-000000000003"),
					filepath.Join(parentDir, "dddd0000-0000-0000-0000-000000000004"),
				),
			},
			want: want{
				dirs: []string{"cccc0000-0000-0000-0000-000000000003"},
			},
		},
		"BothCRDsNotFound": {
			reason: "GC should skip cleanup when both CRDs not found (404).",
			fields: fields{
				kube: &test.MockClient{MockList: test.NewMockListFn(nil, func(obj client.ObjectList) error {
					// Return 404 for both workspace types
					return apierrors.NewNotFound(schema.GroupResource{Group: "tf.upbound.io", Resource: "workspaces"}, "")
				})},
				parentdDir: parentDir,
				fs: withDirs(afero.Afero{Fs: afero.NewMemMapFs()},
					parentDir,
					filepath.Join(parentDir, "some-workspace-uid"),
				),
			},
			want: want{
				dirs: []string{"some-workspace-uid"},
				err:  nil,
			},
		},
		"ClusterForbidden": {
			reason: "GC should abort when cluster workspaces forbidden (403).",
			fields: fields{
				kube: &test.MockClient{MockList: test.NewMockListFn(nil, func(obj client.ObjectList) error {
					switch obj.(type) {
					case *clusterv1beta1.WorkspaceList:
						// Return 403 for cluster workspaces
						return apierrors.NewForbidden(schema.GroupResource{Group: "tf.upbound.io", Resource: "workspaces"}, "", errors.New("forbidden"))
					case *namespacedv1beta1.WorkspaceList:
						return nil
					}
					return nil
				})},
				parentdDir: parentDir,
				fs: withDirs(afero.Afero{Fs: afero.NewMemMapFs()},
					parentDir,
					filepath.Join(parentDir, "some-workspace-uid"),
				),
			},
			want: want{
				dirs: []string{"some-workspace-uid"},
				err:  apierrors.NewForbidden(schema.GroupResource{Group: "tf.upbound.io", Resource: "workspaces"}, "", errors.New("forbidden")),
			},
		},
		"NamespacedForbidden": {
			reason: "GC should abort when namespaced workspaces forbidden (403).",
			fields: fields{
				kube: &test.MockClient{MockList: test.NewMockListFn(nil, func(obj client.ObjectList) error {
					switch obj.(type) {
					case *clusterv1beta1.WorkspaceList:
						return nil
					case *namespacedv1beta1.WorkspaceList:
						// Return 403 for namespaced workspaces
						return apierrors.NewForbidden(schema.GroupResource{Group: "tf.m.upbound.io", Resource: "workspaces"}, "", errors.New("forbidden"))
					}
					return nil
				})},
				parentdDir: parentDir,
				fs: withDirs(afero.Afero{Fs: afero.NewMemMapFs()},
					parentDir,
					filepath.Join(parentDir, "some-workspace-uid"),
				),
			},
			want: want{
				dirs: []string{"some-workspace-uid"},
				err:  apierrors.NewForbidden(schema.GroupResource{Group: "tf.m.upbound.io", Resource: "workspaces"}, "", errors.New("forbidden")),
			},
		},
		"OtherAPIError": {
			reason: "GC should abort on other API errors (network issues, etc.).",
			fields: fields{
				kube: &test.MockClient{MockList: test.NewMockListFn(nil, func(obj client.ObjectList) error {
					switch obj.(type) {
					case *clusterv1beta1.WorkspaceList:
						// Return generic error (simulating network issue)
						return errors.New("connection refused")
					case *namespacedv1beta1.WorkspaceList:
						return nil
					}
					return nil
				})},
				parentdDir: parentDir,
				fs: withDirs(afero.Afero{Fs: afero.NewMemMapFs()},
					parentDir,
					filepath.Join(parentDir, "some-workspace-uid"),
				),
			},
			want: want{
				dirs: []string{"some-workspace-uid"},
				err:  errors.New("connection refused"),
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gc := NewGarbageCollector(tc.fields.kube, tc.fields.parentdDir, WithFs(tc.fields.fs))
			err := gc.collect(tc.args.ctx)
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("gc.collect(...): -want error, +got error:\n%s", diff)
			}

			got := getDirs(tc.fields.fs, tc.fields.parentdDir)
			if diff := cmp.Diff(tc.want.dirs, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("gc.collect(...): -want dirs, +got dirs:\n%s", diff)
			}
		})
	}

}

func TestCollectWithShardName(t *testing.T) {
	parentDir := "/test"

	type fields struct {
		kube       client.Client
		parentdDir string
		fs         afero.Afero
		shardName  string
	}
	type args struct {
		ctx context.Context
	}
	type want struct {
		dirs []string
		err  error
	}
	cases := map[string]struct {
		reason string
		fields fields
		args   args
		want   want
	}{
		"ShardedGCListsAllWorkspaces": {
			reason: "When running in sharded mode, the GC should still list ALL workspaces and only delete directories for workspaces that no longer exist in any shard.",
			fields: fields{
				kube: &test.MockClient{MockList: test.NewMockListFn(nil, func(obj client.ObjectList) error {
					switch v := obj.(type) {
					case *clusterv1beta1.WorkspaceList:
						// Return workspaces from multiple shards
						*v = clusterv1beta1.WorkspaceList{Items: []clusterv1beta1.Workspace{
							{ObjectMeta: metav1.ObjectMeta{
								UID:    types.UID("aaaa0000-0000-0000-0000-000000000001"),
								Labels: map[string]string{ShardLabel: "shard-0"},
							}},
							{ObjectMeta: metav1.ObjectMeta{
								UID:    types.UID("bbbb0000-0000-0000-0000-000000000002"),
								Labels: map[string]string{ShardLabel: "shard-1"},
							}},
						}}
					case *namespacedv1beta1.WorkspaceList:
						*v = namespacedv1beta1.WorkspaceList{}
					}
					return nil
				})},
				parentdDir: parentDir,
				shardName:  "shard-0",
				fs: withDirs(afero.Afero{Fs: afero.NewMemMapFs()},
					parentDir,
					filepath.Join(parentDir, "aaaa0000-0000-0000-0000-000000000001"), // shard-0 workspace
					filepath.Join(parentDir, "bbbb0000-0000-0000-0000-000000000002"), // shard-1 workspace
					filepath.Join(parentDir, "cccc0000-0000-0000-0000-000000000003"), // deleted workspace
				),
			},
			want: want{
				// Both shard-0 and shard-1 directories are preserved.
				// Only the deleted workspace (cccc...) is cleaned up.
				dirs: []string{
					"aaaa0000-0000-0000-0000-000000000001",
					"bbbb0000-0000-0000-0000-000000000002",
				},
			},
		},
		"ShardedGCPreservesOtherShardDirs": {
			reason: "When running in sharded mode, the GC should NOT delete directories for workspaces from other shards.",
			fields: fields{
				kube: &test.MockClient{MockList: test.NewMockListFn(nil, func(obj client.ObjectList) error {
					switch v := obj.(type) {
					case *clusterv1beta1.WorkspaceList:
						*v = clusterv1beta1.WorkspaceList{Items: []clusterv1beta1.Workspace{
							{ObjectMeta: metav1.ObjectMeta{
								UID:    types.UID("aaaa0000-0000-0000-0000-000000000001"),
								Labels: map[string]string{ShardLabel: "shard-0"},
							}},
							{ObjectMeta: metav1.ObjectMeta{
								UID:    types.UID("bbbb0000-0000-0000-0000-000000000002"),
								Labels: map[string]string{ShardLabel: "shard-1"},
							}},
						}}
					case *namespacedv1beta1.WorkspaceList:
						*v = namespacedv1beta1.WorkspaceList{}
					}
					return nil
				})},
				parentdDir: parentDir,
				shardName:  "shard-1",
				fs: withDirs(afero.Afero{Fs: afero.NewMemMapFs()},
					parentDir,
					filepath.Join(parentDir, "aaaa0000-0000-0000-0000-000000000001"), // shard-0
					filepath.Join(parentDir, "bbbb0000-0000-0000-0000-000000000002"), // shard-1
				),
			},
			want: want{
				// Both directories should be preserved because both workspaces still exist
				dirs: []string{
					"aaaa0000-0000-0000-0000-000000000001",
					"bbbb0000-0000-0000-0000-000000000002",
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gc := NewGarbageCollector(tc.fields.kube, tc.fields.parentdDir,
				WithFs(tc.fields.fs),
				WithShardName(tc.fields.shardName),
			)
			err := gc.collect(tc.args.ctx)
			if diff := cmp.Diff(tc.want.err, err, test.EquateErrors()); diff != "" {
				t.Errorf("gc.collect(...): -want error, +got error:\n%s", diff)
			}

			got := getDirs(tc.fields.fs, tc.fields.parentdDir)
			if diff := cmp.Diff(tc.want.dirs, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("gc.collect(...): -want dirs, +got dirs:\n%s", diff)
			}
		})
	}
}

func TestWithShardName(t *testing.T) {
	gc := NewGarbageCollector(nil, "/test", WithShardName("my-shard"))
	if gc.shardName != "my-shard" {
		t.Errorf("WithShardName: expected shardName %q, got %q", "my-shard", gc.shardName)
	}
}

func TestShardLabel(t *testing.T) {
	if ShardLabel != "terraform.crossplane.io/shard" {
		t.Errorf("ShardLabel: expected %q, got %q", "terraform.crossplane.io/shard", ShardLabel)
	}
}
