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

package main

import (
	"os"
	"testing"

	"github.com/alecthomas/kingpin/v2"
	authv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1beta1 "github.com/upbound/provider-terraform/apis/cluster/v1beta1"
	namespacedv1beta1 "github.com/upbound/provider-terraform/apis/namespaced/v1beta1"
)

// TestDefaultLeaderElectionID pins the literal lease name a deployment gets
// when --leader-election-id is left unset. This is the Req 1.4 parity
// guard: today's hardcoded lease ID must not silently change.
func TestDefaultLeaderElectionID(t *testing.T) {
	want := "crossplane-leader-election-provider-terraform"
	if defaultLeaderElectionID != want {
		t.Fatalf("defaultLeaderElectionID = %q, want %q", defaultLeaderElectionID, want)
	}

	app := kingpin.New("test", "")
	id := app.Flag("leader-election-id", "").Default(defaultLeaderElectionID).String()
	if _, err := app.Parse(nil); err != nil {
		t.Fatalf("app.Parse(nil): unexpected error: %v", err)
	}
	if *id != want {
		t.Errorf("parsed --leader-election-id default = %q, want %q", *id, want)
	}
}

// TestBuildSchemeRegistersWorkspaceTypes reproduces the --watch-label-selector
// startup crash if the scheme ordering ever regresses. ctrl.NewManager resolves
// each cache.ByObject key's GVK against the scheme (via s.ObjectKinds); if the
// Workspace types aren't registered by then it fails with "no kind is
// registered for the type v1beta1.Workspace". buildScheme must register them
// up front. It must also re-add the built-ins that controller-runtime's default
// scheme would otherwise supply -- Leases (leader election) and
// SelfSubjectAccessReview (the CRD precheck) -- since supplying our own scheme
// replaces that default.
func TestBuildSchemeRegistersWorkspaceTypes(t *testing.T) {
	s := buildScheme()

	// The exact lookup ctrl.NewManager makes for each ByObject key.
	for _, obj := range []client.Object{
		&clusterv1beta1.Workspace{},
		&namespacedv1beta1.Workspace{},
	} {
		if _, _, err := s.ObjectKinds(obj); err != nil {
			t.Errorf("scheme missing kind for %T: %v", obj, err)
		}
	}

	if !s.Recognizes(coordinationv1.SchemeGroupVersion.WithKind("Lease")) {
		t.Error("built-in Lease missing; leader election would break")
	}
	if !s.Recognizes(authv1.SchemeGroupVersion.WithKind("SelfSubjectAccessReview")) {
		t.Error("built-in SelfSubjectAccessReview missing; the CRD precheck would break")
	}
}

func TestWorkspaceCacheByObject(t *testing.T) {
	cases := map[string]struct {
		selector string
		wantNil  bool
		wantErr  bool
	}{
		"EmptyWatchesAll": {
			// Empty selector must leave the cache unrestricted -- today's
			// behavior (Req 1.4 parity).
			selector: "",
			wantNil:  true,
		},
		"ValidEquality": {
			selector: "sharding.example.com/shard=1",
		},
		"ValidNegation": {
			selector: "!sharding.example.com/shard",
		},
		"ValidSetBasedIn": {
			selector: "sharding.example.com/shard in (0,1)",
		},
		"ValidSetBasedNotIn": {
			selector: "sharding.example.com/shard notin (1,2)",
		},
		"InvalidSelectorSyntax": {
			selector: "===bad===",
			wantErr:  true,
		},
		"InvalidORSyntaxIsRejected": {
			// Kubernetes selectors have no OR combinator (see design doc
			// TD-8 correction). Confirm it's rejected outright rather than
			// silently accepted or misparsed.
			selector: "sharding.example.com/shard==0 OR !sharding.example.com/shard",
			wantErr:  true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := workspaceCacheByObject(tc.selector)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("workspaceCacheByObject(%q): want error, got nil", tc.selector)
				}
				return
			}
			if err != nil {
				t.Fatalf("workspaceCacheByObject(%q): unexpected error: %v", tc.selector, err)
			}

			if tc.wantNil {
				if got != nil {
					t.Errorf("workspaceCacheByObject(%q) = %v, want nil (watch-all)", tc.selector, got)
				}
				return
			}

			if len(got) != 2 {
				t.Fatalf("workspaceCacheByObject(%q): got %d entries, want 2 (cluster + namespaced Workspace)", tc.selector, len(got))
			}

			var sawCluster, sawNamespaced bool
			for obj, byObj := range got {
				if byObj.Label == nil {
					t.Errorf("entry for %T has a nil Label selector", obj)
					continue
				}
				switch obj.(type) {
				case *clusterv1beta1.Workspace:
					sawCluster = true
				case *namespacedv1beta1.Workspace:
					sawNamespaced = true
				default:
					t.Errorf("unexpected object type in cache ByObject map: %T", obj)
				}
			}
			if !sawCluster || !sawNamespaced {
				t.Errorf("workspaceCacheByObject(%q): expected entries for both cluster and namespaced Workspace, sawCluster=%v sawNamespaced=%v", tc.selector, sawCluster, sawNamespaced)
			}
		})
	}
}

// TestWorkspaceCacheByObjectDefaultShardCatchAll proves the documented
// default-instance selector "!shard" (DoesNotExist) matches only Workspaces
// with no shard label at all. There is no "shard 0": any Workspace carrying
// the label -- including shard=0 or the empty string "" -- is excluded from
// the catch-all, per the contract in docs/monolith/Configuration.md.
func TestWorkspaceCacheByObjectDefaultShardCatchAll(t *testing.T) {
	got, err := workspaceCacheByObject("!sharding.example.com/shard")
	if err != nil {
		t.Fatalf("workspaceCacheByObject(%q): unexpected error: %v", "!sharding.example.com/shard", err)
	}

	var sel labels.Selector
	for _, byObj := range got {
		sel = byObj.Label
		break
	}
	if sel == nil {
		t.Fatal("no selector found in workspaceCacheByObject result")
	}

	cases := map[string]struct {
		set  labels.Set
		want bool
	}{
		"AbsentLabelMatches":  {set: labels.Set{}, want: true},
		"Shard0Excluded":      {set: labels.Set{"sharding.example.com/shard": "0"}, want: false},
		"Shard1Excluded":      {set: labels.Set{"sharding.example.com/shard": "1"}, want: false},
		"Shard2Excluded":      {set: labels.Set{"sharding.example.com/shard": "2"}, want: false},
		"EmptyStringExcluded": {set: labels.Set{"sharding.example.com/shard": ""}, want: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := sel.Matches(tc.set); got != tc.want {
				t.Errorf("selector.Matches(%v) = %v, want %v", tc.set, got, tc.want)
			}
		})
	}
}

func TestResolveClaimIdentity(t *testing.T) {
	t.Run("PodNameEnvTakesPrecedence", func(t *testing.T) {
		t.Setenv("POD_NAME", "provider-terraform-shard-1-abc123")
		holder, _ := resolveClaimIdentity("fallback-lease-id")
		if holder != "provider-terraform-shard-1-abc123" {
			t.Errorf("resolveClaimIdentity(%q) holder = %q, want %q", "fallback-lease-id", holder, "provider-terraform-shard-1-abc123")
		}
	})

	t.Run("HolderIdentityNeverEmpty", func(t *testing.T) {
		// Can't portably force os.Hostname() to fail, but the documented
		// precedence guarantees a non-empty result either way: hostname, or
		// (as a last resort) the caller-supplied leaderElectionID.
		t.Setenv("POD_NAME", "")
		holder, _ := resolveClaimIdentity("fallback-lease-id")
		if holder == "" {
			t.Error("holderIdentity should never be empty")
		}
	})

	t.Run("PodNamespaceEnvTakesPrecedence", func(t *testing.T) {
		t.Setenv("POD_NAMESPACE", "tenant-a")
		_, ns := resolveClaimIdentity("fallback-lease-id")
		if ns != "tenant-a" {
			t.Errorf("resolveClaimIdentity(%q) namespace = %q, want %q", "fallback-lease-id", ns, "tenant-a")
		}
	})

	t.Run("EmptyNamespaceWhenNotInCluster", func(t *testing.T) {
		t.Setenv("POD_NAMESPACE", "")
		if _, err := os.Stat(inClusterNamespacePath); err == nil {
			t.Skip("running somewhere with an in-cluster namespace file; precedence is already covered above")
		}
		_, ns := resolveClaimIdentity("fallback-lease-id")
		if ns != "" {
			t.Errorf("namespace = %q, want empty (not running in-cluster, no override set)", ns)
		}
	})
}
