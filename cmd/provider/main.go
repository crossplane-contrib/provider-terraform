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
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	changelogsv1alpha1 "github.com/crossplane/crossplane-runtime/v2/apis/changelogs/proto/v1alpha1"
	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/gate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/customresourcesgate"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/statemetrics"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	sourcev1beta2 "github.com/fluxcd/source-controller/api/v1beta2"
	apiscluster "github.com/upbound/provider-terraform/apis/cluster"
	apisnamespaced "github.com/upbound/provider-terraform/apis/namespaced"
	zapuber "go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	clusterv1beta1 "github.com/upbound/provider-terraform/apis/cluster/v1beta1"
	namespacedv1beta1 "github.com/upbound/provider-terraform/apis/namespaced/v1beta1"
	"github.com/upbound/provider-terraform/internal/bootcheck"
	"github.com/upbound/provider-terraform/internal/claims"
	clusterworkspace "github.com/upbound/provider-terraform/internal/controller/cluster"
	"github.com/upbound/provider-terraform/internal/controller/cluster/workspace"
	"github.com/upbound/provider-terraform/internal/controller/gc"
	namespacedworkspace "github.com/upbound/provider-terraform/internal/controller/namespaced"
	"github.com/upbound/provider-terraform/internal/features"
	authv1 "k8s.io/api/authorization/v1"
)

func init() {
	err := bootcheck.CheckEnv()
	if err != nil {
		log.Fatalf("bootcheck failed. provider will not be started: %v", err)
	}
}

// defaultLeaderElectionID is today's hardcoded lease name, predating the
// --leader-election-id flag. A deployment that leaves the flag unset must
// keep getting exactly this value (Req 1.4 parity).
const defaultLeaderElectionID = "crossplane-leader-election-provider-terraform"

func main() {
	var (
		app                      = kingpin.New(filepath.Base(os.Args[0]), "Terraform support for Crossplane.").DefaultEnvars()
		debug                    = app.Flag("debug", "Run with debug logging.").Short('d').Bool()
		syncInterval             = app.Flag("sync", "Sync interval controls how often all resources will be double checked for drift.").Short('s').Default("1h").Duration()
		pollInterval             = app.Flag("poll", "Poll interval controls how often an individual resource should be checked for drift.").Default("10m").Duration()
		pollStateMetricInterval  = app.Flag("poll-state-metric", "State metric recording interval").Default("5s").Duration()
		pollJitter               = app.Flag("poll-jitter", "If non-zero, varies the poll interval by a random amount up to plus-or-minus this value.").Default("1m").Duration()
		timeout                  = app.Flag("timeout", "Controls how long Terraform processes may run before they are killed.").Default("20m").Duration()
		leaderElection           = app.Flag("leader-election", "Use leader election for the controller manager.").Short('l').Default("false").Envar("LEADER_ELECTION").Bool()
		leaderElectionID         = app.Flag("leader-election-id", "Name of the leader election lease. Set a distinct value per instance so leaders of different instances run concurrently.").Default(defaultLeaderElectionID).Envar("LEADER_ELECTION_ID").String()
		watchLabelSelector       = app.Flag("watch-label-selector", "Restrict the manager cache to Workspaces matching this label selector. Empty (default) watches all Workspaces.").Default("").Envar("WATCH_LABEL_SELECTOR").String()
		maxReconcileRate         = app.Flag("max-reconcile-rate", "The maximum number of concurrent reconciliation operations.").Default("1").Int()
		enableManagementPolicies = app.Flag("enable-management-policies", "Enable support for Management Policies.").Default("true").Envar("ENABLE_MANAGEMENT_POLICIES").Bool()
		enableChangeLogs         = app.Flag("enable-changelogs", "Enable support for capturing change logs during reconciliation.").Default("false").Envar("ENABLE_CHANGE_LOGS").Bool()
		changelogsSocketPath     = app.Flag("changelogs-socket-path", "Path for changelogs socket (if enabled)").Default("/var/run/changelogs/changelogs.sock").Envar("CHANGELOGS_SOCKET_PATH").String()
		logEncoding              = app.Flag("log-encoding", "Container logging output ending. Possible values: console, json").Default("console").Enum("console", "json")
		enableOwnershipClaims    = app.Flag("enable-ownership-claims", "Guard every Terraform run with a per-Workspace ownership claim, for safe handover when a Workspace is relabeled between instances.").Default("false").Envar("ENABLE_OWNERSHIP_CLAIMS").Bool()
		ownershipClaimTTL        = app.Flag("ownership-claim-ttl", "How long an ownership claim's heartbeat may go stale before another instance may steal it.").Default("90s").Envar("OWNERSHIP_CLAIM_TTL").Duration()
		ownershipHeartbeat       = app.Flag("ownership-heartbeat-interval", "How often a held ownership claim's heartbeat is renewed while Terraform is running.").Default("30s").Envar("OWNERSHIP_HEARTBEAT_INTERVAL").Duration()
	)
	kingpin.MustParse(app.Parse(os.Args[1:]))

	workspaceCache, err := workspaceCacheByObject(*watchLabelSelector)
	kingpin.FatalIfError(err, "Cannot parse --watch-label-selector value %q", *watchLabelSelector)

	holderIdentity, claimNamespace := resolveClaimIdentity(*leaderElectionID)
	if *enableOwnershipClaims && claimNamespace == "" {
		kingpin.Fatalf("Cannot resolve this instance's namespace for --enable-ownership-claims: set $POD_NAMESPACE via the pod's downward API, or run in-cluster")
	}
	claimCfg := claims.Config{
		Enabled:           *enableOwnershipClaims,
		TTL:               *ownershipClaimTTL,
		HeartbeatInterval: *ownershipHeartbeat,
		HolderIdentity:    holderIdentity,
		Namespace:         claimNamespace,
	}

	var logEncoder zap.Opts
	switch *logEncoding {
	case "json":
		logEncoder = UseJSON()
	case "console":
		logEncoder = UseISO8601()
	default:
		kingpin.Fatalf("Unknown --log-encoding value: %s. Supported values are 'console' and 'json'", *logEncoding)
	}
	zl := zap.New(zap.UseDevMode(*debug), logEncoder)
	log := logging.NewLogrLogger(zl.WithName("provider-terraform"))
	// SetLogger is required starting in controller-runtime 0.15.0.
	// https://github.com/kubernetes-sigs/controller-runtime/pull/2317
	ctrl.SetLogger(zl)

	log.Debug("Starting",
		"sync-period", syncInterval.String(),
		"poll-interval", pollInterval.String(),
		"poll-jitter", pollJitter.String(),
		"max-reconcile-rate", *maxReconcileRate)

	cfg, err := ctrl.GetConfig()
	kingpin.FatalIfError(err, "Cannot get API server rest config")

	scheme := buildScheme()

	mgr, err := ctrl.NewManager(ratelimiter.LimitRESTConfig(cfg, *maxReconcileRate), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			SyncPeriod: syncInterval,
			ByObject:   workspaceCache,
		},

		// controller-runtime uses both ConfigMaps and Leases for leader
		// election by default. Leases expire after 15 seconds, with a
		// 10 seconds renewal deadline. We've observed leader loss due to
		// renewal deadlines being exceeded when under high load - i.e.
		// hundreds of reconciles per second and ~200rps to the API
		// server. Switching to Leases only and longer leases appears to
		// alleviate this.
		LeaderElection:             *leaderElection,
		LeaderElectionID:           *leaderElectionID,
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		LeaseDuration:              func() *time.Duration { d := 60 * time.Second; return &d }(),
		RenewDeadline:              func() *time.Duration { d := 50 * time.Second; return &d }(),
	})
	kingpin.FatalIfError(err, "Cannot create controller manager")

	metricRecorder := managed.NewMRMetricRecorder()
	stateMetrics := statemetrics.NewMRStateMetrics()

	metrics.Registry.MustRegister(metricRecorder)
	metrics.Registry.MustRegister(stateMetrics)

	ctx := context.Background()
	clusterOpts := controller.Options{
		Logger:                  log,
		MaxConcurrentReconciles: *maxReconcileRate,
		PollInterval:            *pollInterval,
		GlobalRateLimiter:       ratelimiter.NewGlobal(*maxReconcileRate),
		Features:                &feature.Flags{},
		MetricOptions: &controller.MetricOptions{
			PollStateMetricInterval: *pollStateMetricInterval,
			MRMetrics:               metricRecorder,
			MRStateMetrics:          stateMetrics,
		},
	}
	namespacedOpts := controller.Options{
		Logger:                  log,
		MaxConcurrentReconciles: *maxReconcileRate,
		PollInterval:            *pollInterval,
		GlobalRateLimiter:       ratelimiter.NewGlobal(*maxReconcileRate),
		Features:                &feature.Flags{},
		MetricOptions: &controller.MetricOptions{
			PollStateMetricInterval: *pollStateMetricInterval,
			MRMetrics:               metricRecorder,
			MRStateMetrics:          stateMetrics,
		},
	}

	if *enableManagementPolicies {
		clusterOpts.Features.Enable(features.EnableBetaManagementPolicies)
		namespacedOpts.Features.Enable(features.EnableBetaManagementPolicies)
		log.Info("Beta feature enabled", "flag", features.EnableBetaManagementPolicies)
	}

	if *enableChangeLogs {
		clusterOpts.Features.Enable(feature.EnableAlphaChangeLogs)
		namespacedOpts.Features.Enable(feature.EnableAlphaChangeLogs)
		log.Info("Alpha feature enabled", "flag", feature.EnableAlphaChangeLogs)

		conn, err := grpc.NewClient("unix://"+*changelogsSocketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
		kingpin.FatalIfError(err, "failed to create change logs client connection at %s", *changelogsSocketPath)

		clo := controller.ChangeLogOptions{
			ChangeLogger: managed.NewGRPCChangeLogger(changelogsv1alpha1.NewChangeLogServiceClient(conn)),
		}
		clusterOpts.ChangeLogOptions = &clo
		namespacedOpts.ChangeLogOptions = &clo
	}

	// NOTE: cluster-scoped and namespaced Workspaces share a common
	// workspace root directory. Update GC setup if they diverge
	kingpin.FatalIfError(gc.Setup(mgr, workspace.GetTerraformDir(), log), "cannot setup Workspace garbage collector controller")
	canSafeStart, err := canWatchCRD(ctx, mgr)
	kingpin.FatalIfError(err, "SafeStart precheck failed")
	if canSafeStart {
		crdGate := new(gate.Gate[schema.GroupVersionKind])
		clusterOpts.Gate = crdGate
		namespacedOpts.Gate = crdGate
		kingpin.FatalIfError(customresourcesgate.Setup(mgr, namespacedOpts), "Cannot setup CRD gate")
		kingpin.FatalIfError(clusterworkspace.SetupGated(mgr, clusterOpts, *timeout, *pollJitter, claimCfg), "Cannot setup cluster-scoped Workspace controllers")
		kingpin.FatalIfError(namespacedworkspace.SetupGated(mgr, namespacedOpts, *timeout, *pollJitter, claimCfg), "Cannot setup namespaced Workspace controllers")
	} else {
		log.Info("Provider has missing RBAC permissions for watching CRDs, controller SafeStart capability will be disabled")
		kingpin.FatalIfError(clusterworkspace.Setup(mgr, clusterOpts, *timeout, *pollJitter, claimCfg), "Cannot setup cluster-scoped Workspace controllers")
		kingpin.FatalIfError(namespacedworkspace.Setup(mgr, namespacedOpts, *timeout, *pollJitter, claimCfg), "Cannot setup namespaced Workspace controllers")
	}
	kingpin.FatalIfError(mgr.Start(ctrl.SetupSignalHandler()), "Cannot start controller manager")
}

// buildScheme assembles the scheme before the manager exists, so the cache's
// ByObject GVK resolution (which runs inside ctrl.NewManager) can find the
// Workspace kinds. A supplied scheme replaces controller-runtime's default
// client-go scheme, so the built-ins (Leases for leader election,
// SelfSubjectAccessReview for the CRD precheck) must be re-added here too.
func buildScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	kingpin.FatalIfError(clientgoscheme.AddToScheme(s), "Cannot add client-go APIs to scheme")
	kingpin.FatalIfError(apiscluster.AddToScheme(s), "Cannot add terraform APIs to scheme")
	kingpin.FatalIfError(apisnamespaced.AddToScheme(s), "Cannot add terraform APIs to scheme")
	kingpin.FatalIfError(sourcev1.AddToScheme(s), "Cannot add flux gitrepository APIs to scheme")
	kingpin.FatalIfError(sourcev1beta2.AddToScheme(s), "Cannot add flux ocirepository APIs to scheme")
	kingpin.FatalIfError(apiextensionsv1.AddToScheme(s), "Cannot register k8s apiextensions APIs to scheme")
	kingpin.FatalIfError(authv1.AddToScheme(s), "Cannot register k8s authorization APIs to scheme")
	return s
}

// workspaceCacheByObject scopes the manager cache to Workspaces matching
// selector, restricting *only* the cluster-scoped and namespaced Workspace
// types. An empty selector returns a nil map, leaving the cache unrestricted
// for every type (today's behavior). A non-empty selector must not be
// applied via cache.Options.DefaultLabelSelector: that field filters every
// cached type (ProviderConfigs, Secrets, Leases, CRDs), not just Workspaces.
func workspaceCacheByObject(selector string) (map[client.Object]cache.ByObject, error) {
	if selector == "" {
		return nil, nil
	}
	sel, err := labels.Parse(selector)
	if err != nil {
		return nil, err
	}
	return map[client.Object]cache.ByObject{
		&clusterv1beta1.Workspace{}:    {Label: sel},
		&namespacedv1beta1.Workspace{}: {Label: sel},
	}, nil
}

// inClusterNamespacePath is where a pod's own namespace is projected by the
// service account token volume -- the same source controller-runtime itself
// reads for LeaderElectionNamespace when that option is left unset.
const inClusterNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// resolveClaimIdentity determines this instance's ownership-claim holder
// identity and the namespace its claim Leases for *cluster-scoped*
// Workspaces live in (namespaced Workspaces' claims live in their own
// namespace instead -- see claims.Config.Namespace).
//
// HolderIdentity precedence: $POD_NAME (downward API) > hostname >
// leaderElectionID. The last resort falls back to the lease ID rather than
// an empty string because --leader-election-id is already required to be
// unique per instance (TD-2), which makes it a reasonable identity when no
// pod identity is available (e.g. running outside a pod).
//
// Namespace precedence: $POD_NAMESPACE (downward API) > the in-cluster
// service account namespace file.
func resolveClaimIdentity(leaderElectionID string) (holderIdentity, namespace string) {
	holderIdentity = os.Getenv("POD_NAME")
	if holderIdentity == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			holderIdentity = h
		}
	}
	if holderIdentity == "" {
		holderIdentity = leaderElectionID
	}

	namespace = os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		if b, err := os.ReadFile(inClusterNamespacePath); err == nil {
			namespace = strings.TrimSpace(string(b))
		}
	}
	return holderIdentity, namespace
}

// UseISO8601 sets the logger to use ISO8601 timestamp format
func UseISO8601() zap.Opts {
	return func(o *zap.Options) {
		o.TimeEncoder = zapcore.ISO8601TimeEncoder
	}
}

// UseJSON sets the logger to use JSON encoding
func UseJSON() zap.Opts {
	return func(o *zap.Options) {
		encoderConfig := zapuber.NewProductionEncoderConfig()
		encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		o.Encoder = zapcore.NewJSONEncoder(encoderConfig)
	}
}

func canWatchCRD(ctx context.Context, mgr manager.Manager) (bool, error) {
	verbs := []string{"get", "list", "watch"}
	for _, verb := range verbs {
		sar := &authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authv1.ResourceAttributes{
					Group:    "apiextensions.k8s.io",
					Resource: "customresourcedefinitions",
					Verb:     verb,
				},
			},
		}
		if err := mgr.GetClient().Create(ctx, sar); err != nil {
			return false, errors.Wrapf(err, "unable to perform RBAC check for verb %s on CustomResourceDefinitions", verbs)
		}
		if !sar.Status.Allowed {
			return false, nil
		}
	}
	return true, nil
}
