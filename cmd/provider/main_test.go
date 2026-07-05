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
	"testing"

	"github.com/alecthomas/kingpin/v2"
	"k8s.io/apimachinery/pkg/labels"

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
		t.Fatalf("unexpected parse error: %v", err)
	}
	if *id != want {
		t.Errorf("parsed --leader-election-id default = %q, want %q", *id, want)
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
			selector: "sharding.gwcp.guidewire.com/shard=1",
		},
		"ValidNegation": {
			selector: "!sharding.gwcp.guidewire.com/shard",
		},
		"ValidSetBasedIn": {
			selector: "sharding.gwcp.guidewire.com/shard in (0,1)",
		},
		"ValidSetBasedNotIn": {
			selector: "sharding.gwcp.guidewire.com/shard notin (1,2)",
		},
		"InvalidSelectorSyntax": {
			selector: "===bad===",
			wantErr:  true,
		},
		"InvalidORSyntaxIsRejected": {
			// Kubernetes selectors have no OR combinator (see design doc
			// TD-8 correction). Confirm it's rejected outright rather than
			// silently accepted or misparsed.
			selector: "sharding.gwcp.guidewire.com/shard==0 OR !sharding.gwcp.guidewire.com/shard",
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

// TestWorkspaceCacheByObjectDefaultShardCatchAll proves the corrected TD-8
// default-instance selector: "shard notin (1,2,...,N)" matches shard=0 and
// an absent label, and excludes every other shard. The design doc originally
// described this as "shard==0 OR !shard", which is not valid Kubernetes
// selector syntax -- Kubernetes selectors have no OR combinator. NotIn (and
// != ) match an absent label by definition, which is what makes the
// catch-all work.
func TestWorkspaceCacheByObjectDefaultShardCatchAll(t *testing.T) {
	got, err := workspaceCacheByObject("sharding.gwcp.guidewire.com/shard notin (1,2)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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
		"AbsentLabelMatches": {set: labels.Set{}, want: true},
		"Shard0Matches":      {set: labels.Set{"sharding.gwcp.guidewire.com/shard": "0"}, want: true},
		"Shard1Excluded":     {set: labels.Set{"sharding.gwcp.guidewire.com/shard": "1"}, want: false},
		"Shard2Excluded":     {set: labels.Set{"sharding.gwcp.guidewire.com/shard": "2"}, want: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := sel.Matches(tc.set); got != tc.want {
				t.Errorf("selector.Matches(%v) = %v, want %v", tc.set, got, tc.want)
			}
		})
	}
}
