/*
Copyright 2026 The KubeVela Authors.

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

package app

import (
	"flag"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/oam-dev/kubevela/apis/types"
	oamcontroller "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1beta1/spokecluster"
	"github.com/oam-dev/kubevela/pkg/features"
	"github.com/oam-dev/kubevela/pkg/multicluster"
	"github.com/oam-dev/kubevela/pkg/spokecluster/credential"
	common "github.com/oam-dev/kubevela/pkg/utils/common"
	webhookspokecluster "github.com/oam-dev/kubevela/pkg/webhook/core.oam.dev/v1beta1/spokecluster"

	clustercommon "github.com/oam-dev/cluster-gateway/pkg/common"
	toolscache "k8s.io/client-go/tools/cache"
)

// options holds the flags for the vela-cluster-core manager.
type options struct {
	metricsAddr          string
	healthAddr           string
	enableLeaderElection bool
	leaderElectionNS     string
	leaseDuration        time.Duration
	renewDeadline        time.Duration
	retryPeriod          time.Duration
	useWebhook           bool
	webhookPort          int
	certDir              string
	concurrentReconciles int
	autoUpgradeSecret    bool
	credentialCacheTTL   time.Duration
	gatewaySecretNS      string
	denyPrivateEndpoints bool
}

// defaultOptions returns the options with their documented defaults.
func defaultOptions() *options {
	return &options{
		metricsAddr:          ":8080",
		healthAddr:           ":9440",
		enableLeaderElection: false,
		leaderElectionNS:     "",
		leaseDuration:        30 * time.Second,
		renewDeadline:        20 * time.Second,
		retryPeriod:          5 * time.Second,
		useWebhook:           false,
		webhookPort:          9445,
		certDir:              "/k8s-webhook-server/serving-certs",
		concurrentReconciles: 5,
		autoUpgradeSecret:    false,
		credentialCacheTTL:   spokecluster.DefaultCredentialCacheTTL,
	}
}

// addFlags binds the options and the mutable feature-gate flag to fs.
func addFlags(fs *pflag.FlagSet, o *options) {
	fs.StringVar(&o.metricsAddr, "metrics-bind-address", o.metricsAddr,
		"The address the metric endpoint binds to.")
	fs.StringVar(&o.healthAddr, "health-probe-bind-address", o.healthAddr,
		"The address the health probe endpoint binds to.")
	fs.BoolVar(&o.enableLeaderElection, "enable-leader-election", o.enableLeaderElection,
		"Enable leader election for the vela-cluster-core manager. Enabling this will ensure there is only one active manager.")
	fs.StringVar(&o.leaderElectionNS, "leader-election-namespace", o.leaderElectionNS,
		"Determines the namespace in which the leader election lease will be created. Defaults to the pod namespace.")
	fs.DurationVar(&o.leaseDuration, "leader-election-lease-duration", o.leaseDuration,
		"The duration that non-leader candidates will wait to force acquire leadership")
	fs.DurationVar(&o.renewDeadline, "leader-election-renew-deadline", o.renewDeadline,
		"The duration that the acting controlplane will retry refreshing leadership before giving up")
	fs.DurationVar(&o.retryPeriod, "leader-election-retry-period", o.retryPeriod,
		"The duration the LeaderElector clients should wait between tries of actions")
	fs.BoolVar(&o.useWebhook, "use-webhook", o.useWebhook,
		"Enable the SpokeCluster admission webhooks.")
	fs.IntVar(&o.webhookPort, "webhook-port", o.webhookPort,
		"The port the webhook server binds to.")
	fs.StringVar(&o.certDir, "webhook-cert-dir", o.certDir,
		"The directory containing the webhook server's TLS certificate and key.")
	fs.IntVar(&o.concurrentReconciles, "concurrent-reconciles", o.concurrentReconciles,
		"The number of concurrent reconciles for cluster-infrastructure controllers.")
	fs.BoolVar(&o.autoUpgradeSecret, "auto-upgrade-cluster-secret", o.autoUpgradeSecret,
		"Automatically upgrade legacy cluster-gateway secrets on startup.")
	fs.DurationVar(&o.credentialCacheTTL, "credential-cache-ttl", o.credentialCacheTTL,
		"The longest a materialized spoke credential may be reused before it is re-derived. "+
			"A credential is reused only until its own refresh deadline anyway, so this is an upper "+
			"bound rather than the usual lifetime. Set to 0 to disable caching and re-derive on every "+
			"reconcile, which for AWS spokes means an sts:AssumeRole and an eks:DescribeCluster per pass.")
	fs.StringVar(&o.gatewaySecretNS, "cluster-gateway-secret-namespace", o.gatewaySecretNS,
		"Namespace of cluster-gateway Secrets. Must match the Role that grants secrets list/watch. "+
			"The chart sets this to the release namespace. When set, it wins over both the package "+
			"default (vela-system) and the namespace discovered from the cluster-gateway APIService, "+
			"so a failed or late Initialize cannot point the Secret informer at a namespace the Role does not cover.")
	fs.BoolVar(&o.denyPrivateEndpoints, "spoke-endpoint-deny-private", o.denyPrivateEndpoints,
		"When true, refuse SpokeCluster endpoints (and kubeconfig servers) that resolve to RFC1918 or "+
			"CGNAT addresses. Off by default so k3d/kind and VPC-peered private API endpoints keep working. "+
			"Enable on hubs that should only dial public spoke APIs.")

	utilfeature.DefaultMutableFeatureGate.AddFlag(fs)
}

// NewClusterCoreCommand builds the vela-cluster-core cobra command.
func NewClusterCoreCommand() *cobra.Command {
	o := defaultOptions()
	cmd := &cobra.Command{
		Use:          "vela-cluster-core",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(o)
		},
	}
	addFlags(cmd.Flags(), o)

	klogFlags := flag.NewFlagSet("klog", flag.ContinueOnError)
	klog.InitFlags(klogFlags)
	cmd.Flags().AddGoFlagSet(klogFlags)

	return cmd
}

func configureSpokeClusterWebhooks(o *options, waitForCert func() error, registerValidating, registerMutating func()) error {
	if !o.useWebhook {
		return nil
	}
	if !utilfeature.DefaultMutableFeatureGate.Enabled(features.EnableClusterInfrastructure) {
		klog.InfoS("Skipping SpokeCluster admission webhooks because EnableClusterInfrastructure is off")
		return nil
	}

	klog.InfoS("Waiting for SpokeCluster webhook certificate", "certDir", o.certDir)
	if err := waitForCert(); err != nil {
		klog.ErrorS(err, "Unable to start SpokeCluster admission webhooks")
		return err
	}
	registerValidating()
	registerMutating()
	klog.InfoS("Registered SpokeCluster admission webhooks")
	return nil
}

// run is the whole vela-cluster-core lifecycle: build the manager, detect cluster-gateway
// (non-fatal), register the SpokeCluster controller and admission webhooks, register health
// checks, and block on Start.
//
// Both the controller and the webhooks are gated on the SpokeCluster feature: with the gate
// off this binary starts, serves its health endpoints, and reconciles nothing, which is what
// makes the CRD safe to ship ahead of the feature being switched on.
func run(o *options) error {
	ctrl.SetLogger(textlogger.NewLogger(textlogger.NewConfig()))

	restConfig := ctrl.GetConfigOrDie()

	// Resolve the gateway Secret namespace before building the manager cache. Without a
	// namespace-scoped Secret informer, controller-runtime tries to watch Secrets
	// cluster-wide and needs secrets list/watch ClusterRole verbs that RBAC-01 removes.
	if _, err := multicluster.Initialize(restConfig, o.autoUpgradeSecret); err != nil {
		klog.ErrorS(err, "Failed to detect cluster-gateway; spoke probes and discovery will fail until it is ready")
	}
	gatewayNS := resolveGatewaySecretNamespace(multicluster.ClusterGatewaySecretNamespace, o.gatewaySecretNS)
	if gatewayNS != multicluster.ClusterGatewaySecretNamespace {
		klog.InfoS("using configured cluster-gateway Secret namespace so the informer matches RBAC",
			"discovered", multicluster.ClusterGatewaySecretNamespace, "configured", gatewayNS)
	}
	multicluster.ClusterGatewaySecretNamespace = gatewayNS
	credential.DenyPrivateEndpoints = o.denyPrivateEndpoints
	if o.denyPrivateEndpoints {
		klog.InfoS("spoke endpoint policy denies RFC1918 and CGNAT addresses (--spoke-endpoint-deny-private)")
	}

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        common.Scheme,
		Metrics:                       metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress:        o.healthAddr,
		LeaderElection:                o.enableLeaderElection,
		LeaderElectionID:              "vela-cluster-core",
		LeaderElectionNamespace:       o.leaderElectionNS,
		LeaseDuration:                 &o.leaseDuration,
		RenewDeadline:                 &o.renewDeadline,
		RetryPeriod:                   &o.retryPeriod,
		LeaderElectionReleaseOnCancel: true,
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			Port:    o.webhookPort,
			CertDir: o.certDir,
		}),
		// RBAC-01: cache Secrets only in the gateway namespace. Source kubeconfig
		// Secrets in tenant namespaces are read via mgr.GetAPIReader() instead.
		//
		// RBAC-03: do not keep unlabeled Secret payloads (admission TLS, etc.) in
		// process memory. Metadata stays so the source-kubeconfig watch still
		// fires for same-ns rotates. A Label selector on credential-type would
		// drop those source Secrets from the informer entirely and undo CACHE-01.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}: {
					Namespaces: map[string]cache.Config{
						gatewayNS: {},
					},
					Transform: stripNonCredentialSecretData,
				},
			},
		},
	})
	if err != nil {
		klog.ErrorS(err, "Unable to create the vela-cluster-core manager")
		return err
	}

	// Setup does its own feature-gate check and registers nothing when the gate is off, but
	// the error still has to be returned: swallowing it would start a manager that looks
	// healthy and silently reconciles nothing.
	if err := spokecluster.Setup(mgr, oamcontroller.Args{ConcurrentReconciles: o.concurrentReconciles}, o.credentialCacheTTL); err != nil {
		klog.ErrorS(err, "Unable to register the SpokeCluster controller")
		return err
	}

	if err := configureSpokeClusterWebhooks(
		o,
		func() error {
			return waitForWebhookCert(o.certDir, webhookCertWaitTimeout, webhookCertPollInterval)
		},
		func() {
			webhookspokecluster.RegisterValidatingHandler(mgr)
		},
		func() {
			webhookspokecluster.RegisterMutatingHandler(mgr)
		},
	); err != nil {
		return err
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		klog.ErrorS(err, "Unable to add health check")
		return err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		klog.ErrorS(err, "Unable to add readiness check")
		return err
	}

	klog.InfoS("Starting vela-cluster-core manager")
	return mgr.Start(ctrl.SetupSignalHandler())
}

// resolveGatewaySecretNamespace picks the Secret namespace the manager cache (and
// therefore the namespaced Role) will watch. A configured value always wins so a
// failed Initialize cannot leave the informer on the package default (vela-system)
// while the Role is in the release namespace.
func resolveGatewaySecretNamespace(discovered, configured string) string {
	if configured != "" {
		return configured
	}
	if discovered != "" {
		return discovered
	}
	return types.DefaultKubeVelaNS
}

// stripNonCredentialSecretData drops Secret.Data for objects that are not
// cluster-gateway credentials. Admission and cluster-gateway TLS Secrets share
// the gateway namespace with connect Secrets; caching their payloads would keep
// those keys in process memory for the life of the informer. Metadata is kept
// so source-kubeconfig watches still enqueue.
func stripNonCredentialSecretData(obj interface{}) (interface{}, error) {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		// DeletedFinalStateUnknown wraps the last known object.
		if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
			return stripNonCredentialSecretData(tombstone.Obj)
		}
		return obj, nil
	}
	if secret.Labels != nil && secret.Labels[clustercommon.LabelKeyClusterCredentialType] != "" {
		return obj, nil
	}
	out := secret.DeepCopy()
	out.Data = nil
	out.StringData = nil
	return out, nil
}
