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

package spokecluster

import (
	"context"
	"fmt"
	"time"

	recorder "github.com/crossplane/crossplane-runtime/pkg/event"
	pkgmulticluster "github.com/kubevela/pkg/multicluster"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	oamctrl "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev"
	"github.com/oam-dev/kubevela/pkg/features"
	"github.com/oam-dev/kubevela/pkg/spokecluster/credential"
	spokeadmission "github.com/oam-dev/kubevela/pkg/webhook/core.oam.dev/v1beta1/spokecluster"
)

const (
	// controllerName names the controller in events and log lines.
	controllerName = "SpokeCluster"

	// defaultProbeInterval and defaultProbeTimeout back up the schema defaults on
	// spec.probeIntervalSeconds and spec.probeTimeoutSeconds. Admission means an admitted
	// object always carries both, so these only ever apply to an object written before the
	// defaults landed, or built directly in a test.
	defaultProbeInterval = 30 * time.Second
	defaultProbeTimeout  = 10 * time.Second

	// minRequeue floors every requeue. A credential whose refresh deadline has already
	// passed would otherwise ask to be reconciled immediately, forever.
	minRequeue = 5 * time.Second

	// probeHeartbeatInterval is how often a healthy spoke may rewrite lastProbeTime
	// (and only that heartbeat) so the LAST PROBE column stays meaningful without
	// an etcd write every probe interval.
	probeHeartbeatInterval = 5 * time.Minute
)

// Condition reasons. These are an operator-facing contract: they are what distinguishes an
// unreachable spoke from a broken credential, so they are stable strings, not prose.
const (
	reasonNoProvider         = "NoProvider"
	reasonSpecInvalid        = "SpecInvalid"
	reasonMaterializeFailed  = "MaterializeFailed"
	reasonMaterialized       = "Materialized"
	reasonRegisterFailed     = "RegisterFailed"
	reasonSecretMaterialized = "SecretMaterialized"
	reasonProbeFailed        = "ProbeFailed"
	reasonProbeSucceeded     = "ProbeSucceeded"
	reasonDiscoveryFailed    = "DiscoveryFailed"
	reasonDiscoveryOK        = "DiscoveryOK"
)

// Reconcile brings one SpokeCluster's status in line with the spoke's live state.
//
// The order is fixed: fetch, deletion dispatch, finalizer, then connect. The finalizer is
// persisted before any external side effect, so a spoke that got as far as a gateway Secret
// always has teardown guaranteed. Adding it does trigger a follow-up reconcile, but this
// pass carries on into the connect sequence rather than returning early, so a first-time
// SpokeCluster reaches Connected in one pass instead of two.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.InfoS("Reconcile SpokeCluster", "spokecluster", klog.KRef(req.Namespace, req.Name))

	sc := &v1beta1.SpokeCluster{}
	if err := r.Get(ctx, req.NamespacedName, sc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sc)
	}

	if !controllerutil.ContainsFinalizer(sc, FinalizerName) {
		controllerutil.AddFinalizer(sc, FinalizerName)
		if err := r.Update(ctx, sc); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileConnect(ctx, sc)
}

// reconcileConnect runs the connect sequence and writes status exactly once, whichever step
// it got to. It stops at the first hard failure: no registration after a credential
// failure, no probe after a failed registration, no discovery after a failed probe, so the
// conditions read as a prefix of the sequence rather than a mix of stale and fresh.
//
// The status copy starts from the object's current status, which is what lets a discovery
// failure keep the last known good clusterInfo instead of blanking it.
func (r *Reconciler) reconcileConnect(ctx context.Context, sc *v1beta1.SpokeCluster) (ctrl.Result, error) {
	status := sc.Status.DeepCopy()
	status.ObservedGeneration = sc.Generation

	// Re-check Phase 1 admission rules even when the validating webhook is Ignore
	// (job-patch bootstrap window) or disabled. A stored provision/adopt/local/azure
	// object must not register a gateway Secret.
	if errs := spokeadmission.Validate(sc); len(errs) > 0 {
		msg := errs.ToAggregate().Error()
		setCondition(status, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionFalse, reasonSpecInvalid, msg)
		markConnectionUnobserved(status, reasonSpecInvalid, msg)
		return r.finish(ctx, sc, status, probeInterval(sc), nil)
	}

	// Every failure before the probe reports Unknown rather than Disconnected: the spoke was
	// never reached, so its reachability is genuinely unobserved this pass. Disconnected
	// would claim we looked and found it down, and leaving a previous Connected in place
	// would keep asserting reachability the controller can no longer see.
	//
	// The Connected *condition* must move to Unknown as well. status.connection alone left
	// kubectl describe showing Connected=True "spoke answered the authenticated probe" after a
	// credential or registration failure.
	provider, err := r.Providers.For(sc.Spec.Credential.Type)
	if err != nil {
		setCondition(status, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionFalse, reasonNoProvider, err.Error())
		markConnectionUnobserved(status, reasonNoProvider, err.Error())
		return r.finish(ctx, sc, status, 0, err)
	}

	// A credential still well short of its refresh deadline is reused rather than
	// re-derived. Every Materialize on the aws arm is an sts:AssumeRole plus an
	// eks:DescribeCluster, so an uncached loop spends two AWS calls per spoke per probe
	// interval to rebuild an endpoint and CA fixed for the cluster's lifetime and a token
	// good for another twelve minutes. It also rewrote the gateway Secret every pass,
	// because a freshly presigned token never repeats byte for byte, making every pass a
	// real etcd write.
	//
	// The margin is one probe interval, so anything served here stays valid past the next
	// scheduled pass rather than merely valid now; see nextRequeue and the probe-failure
	// exit below, which requeues without consulting the refresh deadline. A provider that
	// reports no deadline is never cached at all: see credentialCache.Put.
	materialized, cached := r.credentials.Get(sc, probeInterval(sc))
	if !cached {
		materialized, err = provider.Materialize(ctx, r.secretReader(), sc)
		if err != nil {
			setCondition(status, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionFalse, reasonMaterializeFailed, err.Error())
			markConnectionUnobserved(status, reasonMaterializeFailed, err.Error())
			return r.finish(ctx, sc, status, 0, err)
		}
		r.credentials.Put(sc, materialized)
	}
	setCondition(status, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionTrue, reasonMaterialized,
		"credential materialized for endpoint "+materialized.Endpoint)

	if err := r.register(ctx, sc, materialized); err != nil {
		setCondition(status, v1beta1.SpokeClusterConditionRegistered, metav1.ConditionFalse, reasonRegisterFailed, err.Error())
		markConnectionUnobserved(status, reasonRegisterFailed, err.Error())
		return r.finish(ctx, sc, status, 0, err)
	}
	setCondition(status, v1beta1.SpokeClusterConditionRegistered, metav1.ConditionTrue, reasonSecretMaterialized,
		"gateway secret is up to date")

	latency, probeErr := r.probeSpoke(ctx, sc)
	probedAt := metav1.Now()
	status.LastProbeTime = &probedAt
	if probeErr != nil {
		// The raw error names the hub's own cluster-gateway proxy URL, never the spoke, so
		// reporting it verbatim sends operators to the wrong address. describeProbeFailure
		// puts the unreachable endpoint and the timeout that applied into the message.
		setCondition(status, v1beta1.SpokeClusterConditionConnected, metav1.ConditionFalse, reasonProbeFailed,
			describeProbeFailure(sc, materialized.Endpoint, probeErr))
		status.Connection = v1beta1.ConnectionStateDisconnected
		klog.InfoS("Spoke probe failed", "spokecluster", klog.KObj(sc), "endpoint", materialized.Endpoint,
			"timeout", probeTimeout(sc), "err", probeErr)
		// A 401 is the only probe outcome carrying credential information: the spoke read
		// the token and rejected it. Drop the cached copy so the next pass remints rather
		// than serving the same rejected credential until its refresh deadline, which an
		// out-of-band revocation (role deleted, trust policy narrowed, access entry
		// removed) would otherwise stretch to the full reuse window.
		//
		// Deliberately not any other failure. A 403 means the token authenticated and RBAC
		// refused it, so reminting cannot help. A timeout, a gateway 5xx, a refused
		// connection or a TLS error never tested the credential at all, and evicting on
		// those would turn a regional outage into a synchronized AssumeRole burst against
		// an account-wide STS quota exactly when it can least absorb one.
		//
		// A permanently rejected credential therefore remints once per probe interval,
		// which is what this controller did on every pass before the cache existed.
		if apierrors.IsUnauthorized(probeErr) {
			r.credentials.Invalidate(client.ObjectKeyFromObject(sc))
		}
		// Deliberately no error: an unreachable spoke is state to report, not a controller
		// fault to back off on. The plain probe interval also skips the refresh cap, so a
		// disconnected spoke with an expiring credential can idle up to one interval past
		// expiry; harmless, because the next pass remints before probing.
		return r.finish(ctx, sc, status, probeInterval(sc), nil)
	}
	// The measured latency belongs in clusterInfo.latencyMillis, not in this message: a
	// message that changes every pass would make every status write a real write for no
	// added information, and it is already reported in a field with a printer column.
	setCondition(status, v1beta1.SpokeClusterConditionConnected, metav1.ConditionTrue, reasonProbeSucceeded,
		"spoke answered the authenticated probe")
	status.Connection = v1beta1.ConnectionStateConnected

	info, discoverErr := r.discoverSpoke(ctx, sc, materialized, latency)
	if discoverErr != nil {
		// Inventory is not connectivity: a spoke that answers /apis but refuses a node
		// list is still connected, so this never fails the pass.
		setCondition(status, v1beta1.SpokeClusterConditionInfoSynced, metav1.ConditionFalse, reasonDiscoveryFailed, discoverErr.Error())
		// Discovery runs authenticated API reads. A 401 here means the credential was
		// rejected after the probe succeeded (or the probe path was somehow still
		// anonymous). Drop the cache so the next pass remints; leave 403 alone, because
		// reminting cannot fix missing get/list RBAC on nodes or version.
		if apierrors.IsUnauthorized(discoverErr) {
			r.credentials.Invalidate(client.ObjectKeyFromObject(sc))
		}
	} else {
		// Stamped only here, on success. A skipped pass (probe failed) or a failed discovery
		// leaves the previous value in place, so the gap between this and now is exactly how
		// stale the reported inventory is.
		syncedAt := metav1.Now()
		info.LastSyncedTime = &syncedAt
		setCondition(status, v1beta1.SpokeClusterConditionInfoSynced, metav1.ConditionTrue, reasonDiscoveryOK,
			"cluster inventory refreshed")
		status.ClusterInfo = info
	}

	return r.finish(ctx, sc, status, nextRequeue(sc, materialized), nil)
}

// probeSpoke dispatches to the probe seam when a test set one, and to the real probe
// otherwise.
func (r *Reconciler) probeSpoke(ctx context.Context, sc *v1beta1.SpokeCluster) (time.Duration, error) {
	if r.probeFn != nil {
		return r.probeFn(ctx, sc)
	}
	return r.probe(ctx, sc)
}

// discoverSpoke dispatches to the discovery seam when a test set one, and to the real
// discovery otherwise.
func (r *Reconciler) discoverSpoke(ctx context.Context, sc *v1beta1.SpokeCluster, m *credential.Materialized, latency time.Duration) (*v1beta1.SpokeClusterInfo, error) {
	if r.discoverFn != nil {
		return r.discoverFn(ctx, sc, m, latency)
	}
	return r.discover(ctx, sc, m, latency)
}

// finish ends every pass the same way: write the status that was computed, then either
// surface the failure for controller-runtime's exponential backoff or schedule the next
// pass. Status is written first so a failure is still visible to an operator even though
// the pass errors out.
func (r *Reconciler) finish(ctx context.Context, sc *v1beta1.SpokeCluster, status *v1beta1.SpokeClusterStatus, requeue time.Duration, reconcileErr error) (ctrl.Result, error) {
	prev := sc.Status
	if statusNeedsWrite(prev, *status) {
		if err := r.updateStatus(ctx, sc, status); err != nil {
			return ctrl.Result{}, err
		}
		// Events and counters describe the status that was actually persisted. Emitting
		// first would fire ProbeSucceeded (and increment the transition counter) on a
		// conflict that then retries, so the next successful write looks like a no-op.
		r.emitStatusEvents(sc, &prev, status)
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	if requeue < minRequeue {
		requeue = minRequeue
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// updateStatus replaces status wholesale through the status subresource, retrying on
// conflict. Replacing rather than merging is safe because this controller is the only
// writer of SpokeCluster status: the hub pulls spoke state, the spoke pushes nothing.
func (r *Reconciler) updateStatus(ctx context.Context, sc *v1beta1.SpokeCluster, status *v1beta1.SpokeClusterStatus) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1beta1.SpokeCluster{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(sc), latest); err != nil {
			return err
		}
		latest.Status = *status
		return r.Status().Update(ctx, latest)
	})
}

// statusNeedsWrite reports whether next differs from prev in any operator-facing
// field. latencyMillis and lastSyncedTime still move on every healthy pass and
// are stripped from the compare so they do not force a write alone. lastProbeTime
// is written on the first probe and then at most once per probeHeartbeatInterval
// so LAST PROBE stays a recent probe time without ~2 etcd writes per spoke per
// interval. Connection flips and condition changes always write.
func statusNeedsWrite(prev, next v1beta1.SpokeClusterStatus) bool {
	if !apiequality.Semantic.DeepEqual(statusForCompare(prev), statusForCompare(next)) {
		return true
	}
	return lastProbeNeedsWrite(prev.LastProbeTime, next.LastProbeTime)
}

func statusForCompare(in v1beta1.SpokeClusterStatus) *v1beta1.SpokeClusterStatus {
	out := in.DeepCopy()
	out.LastProbeTime = nil
	if out.ClusterInfo != nil {
		out.ClusterInfo.LastSyncedTime = nil
		out.ClusterInfo.LatencyMillis = 0
	}
	return out
}

func lastProbeNeedsWrite(prev, next *metav1.Time) bool {
	if next == nil {
		return false
	}
	if prev == nil {
		return true
	}
	return next.Time.Sub(prev.Time) >= probeHeartbeatInterval
}

// setCondition records one condition. meta.SetStatusCondition leaves LastTransitionTime
// alone when the status has not flipped, so a steadily connected spoke does not churn its
// conditions on every pass.
func setCondition(status *v1beta1.SpokeClusterStatus, condType string, condStatus metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:    condType,
		Status:  condStatus,
		Reason:  reason,
		Message: message,
	})
}

// markConnectionUnobserved sets status.connection and the Connected condition to Unknown
// when this pass never reached the probe. reason and message name the earlier failure so
// describe stays accurate after a previously Connected spoke loses its credential.
func markConnectionUnobserved(status *v1beta1.SpokeClusterStatus, reason, message string) {
	status.Connection = v1beta1.ConnectionStateUnknown
	setCondition(status, v1beta1.SpokeClusterConditionConnected, metav1.ConditionUnknown, reason, message)
}

// probeInterval is how often a spoke is probed when nothing else forces an earlier pass.
func probeInterval(sc *v1beta1.SpokeCluster) time.Duration {
	if sc.Spec.ProbeIntervalSeconds > 0 {
		return time.Duration(sc.Spec.ProbeIntervalSeconds) * time.Second
	}
	return defaultProbeInterval
}

// nextRequeue is the probe interval, capped by the credential's own refresh deadline. That
// cap is what keeps an AWS spoke reconciling before its minted token expires, rather than
// on whatever cadence someone chose for probing. A static kubeconfig spoke reports a zero
// NextRefresh and so just follows the probe interval.
//
// The result can be zero or negative when the deadline has already passed; finish applies
// the floor.
func nextRequeue(sc *v1beta1.SpokeCluster, m *credential.Materialized) time.Duration {
	requeue := probeInterval(sc)
	if m == nil || m.NextRefresh.IsZero() {
		return requeue
	}
	if untilRefresh := time.Until(m.NextRefresh); untilRefresh < requeue {
		return untilRefresh
	}
	return requeue
}

// ignoreOwnStatusWrites keeps the controller from reacting to the status it just wrote.
//
// Every pass records a fresh lastProbeTime and a freshly measured latency, so every pass
// changes status, and each of those changes comes back as an update event that triggers
// another pass. Left unfiltered that multiplied the work: a single spoke on a 10 second
// probe interval reconciled about 26 times a minute instead of 6, probing the spoke every
// time, and the amplification scales with the fleet.
//
// The filter is written out rather than using predicate.GenerationChangedPredicate because
// generation does not move when a finalizer is added or when a deletion timestamp is set,
// and dropping the deletion event would leave teardown waiting on the next requeue instead
// of starting immediately. Cadence stays with RequeueAfter, which is where this controller
// wants it: status tracks live state on the probe interval, not on how often status changes.
var ignoreOwnStatusWrites = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		if e.ObjectOld == nil || e.ObjectNew == nil {
			return true
		}
		if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
			return true
		}
		// Compare whether the object is deleting, not the timestamp pointers: old and new
		// are distinct objects, so their pointers never match and comparing them directly
		// would let every update through once deletion had started.
		return (e.ObjectOld.GetDeletionTimestamp() != nil) != (e.ObjectNew.GetDeletionTimestamp() != nil)
	},
}

// SetupWithManager registers the reconciler with the manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.record = recorder.NewAPIRecorder(mgr.GetEventRecorderFor(controllerName)).
		WithAnnotations("controller", controllerName)
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.concurrentReconciles,
		}).
		For(&v1beta1.SpokeCluster{}, builder.WithPredicates(ignoreOwnStatusWrites)).
		// Source kubeconfig Secrets in the gateway namespace already sit in the
		// RBAC-01 informer. A cluster-wide Secret watch would need ClusterRole
		// list/watch, which that change removed on purpose. Tenant-namespace
		// rotations still wait for the next probe (APIReader re-reads them).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapKubeconfigSecret),
			builder.WithPredicates(sourceKubeconfigSecretPredicate)).
		Complete(r)
}

// Setup adds the SpokeCluster controller to the manager, unless the feature gate is off, in
// which case nothing is registered and the CRD stays inert.
//
// This runs in the separate vela-cluster-core manager (cmd/cluster-core), never in
// vela-core; see the note on pkg/controller/core.oam.dev/v1beta1.Setup.
// credentialCacheTTL is passed separately from args rather than added to oamctrl.Args,
// which is shared with the vela-core controllers that have no credential cache.
func Setup(mgr ctrl.Manager, args oamctrl.Args, credentialCacheTTL time.Duration) error {
	if !utilfeature.DefaultMutableFeatureGate.Enabled(features.EnableClusterInfrastructure) {
		klog.InfoS("SpokeCluster controller disabled because its feature gate is off",
			"gate", features.EnableClusterInfrastructure)
		return nil
	}
	// The TTL is a ceiling, never an extension: an entry is served until the earlier of it
	// and the credential's own refresh deadline, so a value above the default cannot make
	// any credential live longer than its provider allows today. It does disarm the
	// backstop that stops a provider reporting an implausible deadline from pinning a
	// credential in memory, which is worth saying out loud rather than leaving to be
	// discovered when the azure or gcp arms start reporting deadlines of their own.
	switch {
	case credentialCacheTTL <= 0:
		klog.InfoS("Spoke credential caching is disabled; every reconcile will re-derive its credential, "+
			"which for aws spokes is an sts:AssumeRole and an eks:DescribeCluster per spoke per probe interval",
			"credentialCacheTTL", credentialCacheTTL)
	case credentialCacheTTL > DefaultCredentialCacheTTL:
		// The chart refuses to render above the default, so reaching this means the flag was
		// set directly. Log rather than fail: the value is inert while every provider reports
		// a nearer deadline of its own, and refusing to start would be a harsh response to a
		// setting that changes nothing today.
		klog.InfoS("Spoke credential cache TTL is above the default, weakening the ceiling on how long a "+
			"credential may be reused if a provider ever reports an implausibly distant refresh deadline",
			"credentialCacheTTL", credentialCacheTTL, "default", DefaultCredentialCacheTTL)
	default:
		klog.V(1).InfoS("Spoke credential cache enabled", "credentialCacheTTL", credentialCacheTTL)
	}

	// Uncached and multicluster-aware, for the reason spelled out on Reconciler.SpokeReader.
	spokeReader, err := pkgmulticluster.NewDefaultClient(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return fmt.Errorf("unable to build the multicluster client for spoke discovery: %w", err)
	}
	r := &Reconciler{
		Client:               mgr.GetClient(),
		SpokeReader:          spokeReader,
		SecretReader:         mgr.GetAPIReader(),
		Scheme:               mgr.GetScheme(),
		Config:               mgr.GetConfig(),
		Providers:            credential.DefaultRegistry(),
		concurrentReconciles: args.ConcurrentReconciles,
		// Background rather than a manager context: the cache's sweeper goroutine should
		// live exactly as long as the process, and Setup runs before the manager exposes
		// a context of its own. A non-positive TTL yields a nil cache, which disables
		// caching and restores per-pass materialization.
		credentials: newCredentialCache(context.Background(), credentialCacheTTL),
	}
	if err := mgr.Add(manager.RunnableFunc(r.StartGatewaySecretJanitor)); err != nil {
		return fmt.Errorf("unable to add gateway secret janitor: %w", err)
	}
	return r.SetupWithManager(mgr)
}
