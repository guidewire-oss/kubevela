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
	"errors"
	"fmt"
	"strings"
	"time"

	clustercommon "github.com/oam-dev/cluster-gateway/pkg/common"
	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/spokecluster/credential"
)

// mockProvider is a credential.Provider that returns a canned result. The only fake
// provider in the repo is unexported in package credential, so it cannot be reused here.
type mockProvider struct {
	credType     v1beta1.CredentialType
	materialized *credential.Materialized
	err          error
	// calls counts Materialize invocations. It is the only way to observe the credential
	// cache: a hit and a miss produce identical conditions and an identical gateway Secret
	// by design, so nothing else tells them apart. A pointer receiver is required for the
	// count to survive. Unsynchronized on purpose, since no test drives Reconcile
	// concurrently.
	calls int
}

func (m *mockProvider) Type() v1beta1.CredentialType { return m.credType }

func (m *mockProvider) Materialize(_ context.Context, _ client.Reader, _ *v1beta1.SpokeCluster) (*credential.Materialized, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	// A fresh copy per call, so a caller that mutates one result cannot corrupt the next.
	return m.materialized.DeepCopy(), nil
}

// kubeconfigRegistry is a one-arm registry answering for the kubeconfig credential type.
func kubeconfigRegistry(m *credential.Materialized, err error) credential.Registry {
	reg, _ := kubeconfigRegistryWithProvider(m, err)
	return reg
}

// kubeconfigRegistryWithProvider is kubeconfigRegistry for tests that need to count
// Materialize calls.
func kubeconfigRegistryWithProvider(m *credential.Materialized, err error) (credential.Registry, *mockProvider) {
	p := &mockProvider{
		credType:     v1beta1.CredentialTypeKubeconfig,
		materialized: m,
		err:          err,
	}
	return credential.Registry{v1beta1.CredentialTypeKubeconfig: p}, p
}

// refreshingCredential stands in for the aws arm: a bearer token carrying a refresh
// deadline, which is the only kind the credential cache will hold. tokenCredential has a
// zero NextRefresh and is therefore uncacheable by design.
func refreshingCredential(in time.Duration) *credential.Materialized {
	m := tokenCredential()
	m.NextRefresh = time.Now().Add(in)
	return m
}

// connectableSpoke is a SpokeCluster the loop can carry all the way to Connected: it names
// a kubeconfig credential and sets the probe knobs explicitly, because the fake client
// applies no schema defaulting.
func connectableSpoke(name string) *v1beta1.SpokeCluster {
	sc := spoke(name, v1beta1.SpokeDeletionPolicyDetach)
	sc.Generation = 1
	sc.Spec.Mode = v1beta1.SpokeClusterModeConnect
	sc.Spec.ProbeIntervalSeconds = 30
	sc.Spec.ProbeTimeoutSeconds = 10
	sc.Spec.Credential = v1beta1.CredentialSpec{
		Type: v1beta1.CredentialTypeKubeconfig,
		Kubeconfig: &v1beta1.KubeconfigCredential{
			SecretRef: v1beta1.SecretKeyRef{Name: name + "-kubeconfig", Namespace: sc.Namespace, Key: "kubeconfig"},
		},
	}
	return sc
}

// connectedReconciler wires a spoke that materializes, registers, probes and discovers
// cleanly. The probe and discovery seams keep the test off any live spoke or rest.Config.
func connectedReconciler(t GinkgoTInterface, sc *v1beta1.SpokeCluster) *Reconciler {
	t.Helper()
	r := newTestReconciler(t, sc)
	r.Providers = kubeconfigRegistry(tokenCredential(), nil)
	r.probeFn = func(_ context.Context, _ *v1beta1.SpokeCluster) (time.Duration, error) {
		return 7 * time.Millisecond, nil
	}
	r.discoverFn = func(_ context.Context, _ *v1beta1.SpokeCluster, _ *credential.Materialized, latency time.Duration) (*v1beta1.SpokeClusterInfo, error) {
		return &v1beta1.SpokeClusterInfo{
			KubernetesVersion: "v1.31.5+k3s1",
			LatencyMillis:     latency.Milliseconds(),
		}, nil
	}
	return r
}

func reconcileOnce(t GinkgoTInterface, r *Reconciler, sc *v1beta1.SpokeCluster) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sc)})
}

// readSpoke re-fetches a SpokeCluster so assertions see what was persisted, not the
// in-memory copy the loop mutated.
func readSpoke(t GinkgoTInterface, r *Reconciler, sc *v1beta1.SpokeCluster) *v1beta1.SpokeCluster {
	t.Helper()
	latest := &v1beta1.SpokeCluster{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sc), latest); err != nil {
		t.Fatalf("failed to re-read spokecluster %s: %v", sc.Name, err)
	}
	return latest
}

// wantCondition asserts a condition's status and reason together, since a correct status
// with a drifted reason still breaks the operator-facing contract.
func wantCondition(t GinkgoTInterface, sc *v1beta1.SpokeCluster, condType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	cond := meta.FindStatusCondition(sc.Status.Conditions, condType)
	if cond == nil {
		t.Fatalf("condition %s is absent, want %s/%s", condType, status, reason)
	}
	if cond.Status != status {
		t.Errorf("condition %s status = %s, want %s", condType, cond.Status, status)
	}
	if cond.Reason != reason {
		t.Errorf("condition %s reason = %q, want %q", condType, cond.Reason, reason)
	}
}

// TestReconcileConnectedKubeconfig covers the happy path end to end: the first pass adds
// the finalizer before any external work and still completes the connect sequence in the
// same pass, and the object ends Connected with all four conditions true.
var _ = It("ReconcileConnectedKubeconfig", func() {
	t := GinkgoT()
	sc := connectableSpoke("spoke-happy")
	r := connectedReconciler(t, sc)

	res, err := reconcileOnce(t, r, sc)
	if err != nil {
		t.Fatalf("Reconcile returned an unexpected error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want a positive requeue so status keeps tracking live state", res.RequeueAfter)
	}

	latest := readSpoke(t, r, sc)
	if !containsFinalizer(latest) {
		t.Errorf("finalizer %q was not added on the first pass", FinalizerName)
	}

	wantCondition(t, latest, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionTrue, reasonMaterialized)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionRegistered, metav1.ConditionTrue, reasonSecretMaterialized)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionConnected, metav1.ConditionTrue, reasonProbeSucceeded)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionInfoSynced, metav1.ConditionTrue, reasonDiscoveryOK)

	if latest.Status.Connection != v1beta1.ConnectionStateConnected {
		t.Errorf("status.connection = %q, want %q", latest.Status.Connection, v1beta1.ConnectionStateConnected)
	}
	if latest.Status.LastProbeTime == nil {
		t.Error("status.lastProbeTime was not recorded")
	}
	if latest.Status.ObservedGeneration != sc.Generation {
		t.Errorf("status.observedGeneration = %d, want %d", latest.Status.ObservedGeneration, sc.Generation)
	}
	if latest.Status.ClusterInfo == nil {
		t.Fatal("status.clusterInfo was not populated")
	}
	if latest.Status.ClusterInfo.LastSyncedTime == nil {
		t.Error("clusterInfo.lastSyncedTime must be stamped on a successful discovery; " +
			"without it nothing records how old the inventory is")
	}
	if latest.Status.ClusterInfo.KubernetesVersion != "v1.31.5+k3s1" {
		t.Errorf("clusterInfo.kubernetesVersion = %q, want %q", latest.Status.ClusterInfo.KubernetesVersion, "v1.31.5+k3s1")
	}

	// The gateway Secret is the externally visible half of Registered.
	secret := readGatewaySecret(t, r.Client, sc.Name)
	if string(secret.Data[secretKeyEndpoint]) != "https://spoke.example.com" {
		t.Errorf("gateway secret endpoint = %q, want the materialized endpoint", secret.Data[secretKeyEndpoint])
	}

	// A second pass must converge rather than early-return now that the spoke is connected.
	res, err = reconcileOnce(t, r, sc)
	if err != nil {
		t.Fatalf("second Reconcile returned an unexpected error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("second pass RequeueAfter = %v, want a positive requeue", res.RequeueAfter)
	}
	latest = readSpoke(t, r, sc)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionConnected, metav1.ConditionTrue, reasonProbeSucceeded)
})

// TestReconcileProbeFailureMarksDisconnected is the reason the condition set is split the
// way it is: an unreachable spoke must be distinguishable from a broken credential, and it
// must not be treated as a controller fault.
var _ = It("ReconcileProbeFailureMarksDisconnected", func() {
	t := GinkgoT()
	sc := connectableSpoke("spoke-unreachable")
	r := connectedReconciler(t, sc)
	r.probeFn = func(_ context.Context, _ *v1beta1.SpokeCluster) (time.Duration, error) {
		return 0, errors.New("dial tcp: connection refused")
	}

	res, err := reconcileOnce(t, r, sc)
	if err != nil {
		t.Fatalf("a probe failure must not return an error (it is reported state, not a fault): %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("RequeueAfter = %v, want the probe interval so the spoke is retried", res.RequeueAfter)
	}

	latest := readSpoke(t, r, sc)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionConnected, metav1.ConditionFalse, reasonProbeFailed)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionTrue, reasonMaterialized)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionRegistered, metav1.ConditionTrue, reasonSecretMaterialized)

	if latest.Status.Connection != v1beta1.ConnectionStateDisconnected {
		t.Errorf("status.connection = %q, want %q", latest.Status.Connection, v1beta1.ConnectionStateDisconnected)
	}
	if latest.Status.LastProbeTime == nil {
		t.Error("status.lastProbeTime must be recorded on a failed probe too")
	}
	if meta.FindStatusCondition(latest.Status.Conditions, v1beta1.SpokeClusterConditionInfoSynced) != nil {
		t.Error("discovery must not run after a failed probe")
	}
})

// TestLastSyncedTimeFreezesWhileInventoryIsStale is the staleness contract. InfoSynced stays
// True across a disconnect (discovery is skipped, not failed) and its lastTransitionTime does
// not move either, so lastSyncedTime is the only thing that can tell an operator the reported
// inventory is old. It must therefore stop advancing the moment discovery stops succeeding.
var _ = It("LastSyncedTimeFreezesWhileInventoryIsStale", func() {
	t := GinkgoT()
	tests := map[string]func(r *Reconciler){
		"probe fails so discovery is skipped": func(r *Reconciler) {
			r.probeFn = func(context.Context, *v1beta1.SpokeCluster) (time.Duration, error) {
				return 0, errors.New("dial tcp: connection refused")
			}
		},
		"probe succeeds but discovery fails": func(r *Reconciler) {
			r.discoverFn = func(context.Context, *v1beta1.SpokeCluster, *credential.Materialized, time.Duration) (*v1beta1.SpokeClusterInfo, error) {
				return nil, errors.New("nodes is forbidden")
			}
		},
	}

	for name, breakIt := range tests {
		By(name, func() {
			sc := connectableSpoke("spoke-staleness")
			r := connectedReconciler(t, sc)

			if _, err := reconcileOnce(t, r, sc); err != nil {
				t.Fatalf("first Reconcile returned an unexpected error: %v", err)
			}
			afterSuccess := readSpoke(t, r, sc)
			if afterSuccess.Status.ClusterInfo == nil || afterSuccess.Status.ClusterInfo.LastSyncedTime == nil {
				t.Fatal("first pass did not stamp clusterInfo.lastSyncedTime")
			}
			stamped := *afterSuccess.Status.ClusterInfo.LastSyncedTime

			breakIt(r)
			if _, err := reconcileOnce(t, r, sc); err != nil {
				t.Fatalf("second Reconcile returned an unexpected error: %v", err)
			}

			latest := readSpoke(t, r, sc)
			if latest.Status.ClusterInfo == nil {
				t.Fatal("clusterInfo must be retained, not blanked, when discovery does not succeed")
			}
			if latest.Status.ClusterInfo.LastSyncedTime == nil {
				t.Fatal("clusterInfo.lastSyncedTime must be retained alongside the stale inventory")
			}
			if !latest.Status.ClusterInfo.LastSyncedTime.Equal(&stamped) {
				t.Errorf("lastSyncedTime advanced to %v from %v; it must only move on a successful discovery",
					latest.Status.ClusterInfo.LastSyncedTime, stamped)
			}
		})
	}
})

// TestProbeFailureMessageNamesTheEndpoint pins the operator-facing half of the fix: the
// condition must point at the spoke, not at the hub's own gateway proxy URL.
var _ = It("ProbeFailureMessageNamesTheEndpoint", func() {
	t := GinkgoT()
	sc := connectableSpoke("spoke-message")
	r := connectedReconciler(t, sc)
	r.probeFn = func(context.Context, *v1beta1.SpokeCluster) (time.Duration, error) {
		return 0, fmt.Errorf(`Get "https://10.43.0.1:443/apis/cluster.core.oam.dev/v1alpha1/clustergateways/spoke-message/proxy/healthz": %w`,
			context.DeadlineExceeded)
	}

	if _, err := reconcileOnce(t, r, sc); err != nil {
		t.Fatalf("Reconcile returned an unexpected error: %v", err)
	}

	latest := readSpoke(t, r, sc)
	cond := meta.FindStatusCondition(latest.Status.Conditions, v1beta1.SpokeClusterConditionConnected)
	if cond == nil {
		t.Fatal("Connected condition missing")
	}
	// tokenCredential drives the materialized endpoint the reconciler reports.
	if !strings.Contains(cond.Message, "https://spoke.example.com") {
		t.Errorf("Connected message = %q, want it to name the spoke endpoint", cond.Message)
	}
})

// TestReconcileCredentialFailure checks both halves of a materialization failure: the
// condition is recorded, and the error still surfaces so controller-runtime backs off.
var _ = It("ReconcileCredentialFailure", func() {
	t := GinkgoT()
	cases := map[string]struct {
		registry   credential.Registry
		wantReason string
	}{
		"no provider for the declared credential type": {
			registry:   credential.Registry{},
			wantReason: reasonNoProvider,
		},
		"provider fails to materialize": {
			registry:   kubeconfigRegistry(nil, errors.New("secret vela-system/spoke-kubeconfig not found")),
			wantReason: reasonMaterializeFailed,
		},
	}

	for name, tc := range cases {
		By(name, func() {
			sc := connectableSpoke("spoke-badcred")
			r := connectedReconciler(t, sc)
			r.Providers = tc.registry

			if _, err := reconcileOnce(t, r, sc); err == nil {
				t.Fatal("Reconcile returned nil, want an error so controller-runtime applies backoff")
			}

			latest := readSpoke(t, r, sc)
			wantCondition(t, latest, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionFalse, tc.wantReason)
			if meta.FindStatusCondition(latest.Status.Conditions, v1beta1.SpokeClusterConditionRegistered) != nil {
				t.Error("registration must not run after a credential failure")
			}
		})
	}
})

// TestReconcileCredentialFailureMarksConnectionUnknown separates "we never got far enough to
// know" from "we probed and it was down". It also covers the regression case: a spoke that
// was Connected and then loses its credential must stop asserting reachability on both
// status.connection and the Connected condition.
var _ = It("ReconcileCredentialFailureMarksConnectionUnknown", func() {
	t := GinkgoT()
	cases := map[string]struct {
		registry   credential.Registry
		wantReason string
	}{
		"no provider": {
			registry:   credential.Registry{},
			wantReason: reasonNoProvider,
		},
		"materialize failed": {
			registry:   kubeconfigRegistry(nil, errors.New("kubeconfig is malformed")),
			wantReason: reasonMaterializeFailed,
		},
	}

	for name, tc := range cases {
		By(name, func() {
			sc := connectableSpoke("spoke-unknown")
			sc.Status.Connection = v1beta1.ConnectionStateConnected
			meta.SetStatusCondition(&sc.Status.Conditions, metav1.Condition{
				Type:    v1beta1.SpokeClusterConditionConnected,
				Status:  metav1.ConditionTrue,
				Reason:  reasonProbeSucceeded,
				Message: "spoke answered the healthz probe",
			})
			r := connectedReconciler(t, sc)
			r.Providers = tc.registry

			if _, err := reconcileOnce(t, r, sc); err == nil {
				t.Fatal("Reconcile returned nil, want the credential error")
			}
			latest := readSpoke(t, r, sc)
			if got := latest.Status.Connection; got != v1beta1.ConnectionStateUnknown {
				t.Errorf("status.connection = %q, want %q", got, v1beta1.ConnectionStateUnknown)
			}
			wantCondition(t, latest, v1beta1.SpokeClusterConditionConnected, metav1.ConditionUnknown, tc.wantReason)
		})
	}
})

// TestReconcileRegisterFailure covers the middle step: a Secret this SpokeCluster may not
// adopt stops the sequence at registration and surfaces the error.
var _ = It("ReconcileRegisterFailure", func() {
	t := GinkgoT()
	sc := connectableSpoke("spoke-collision")
	r := newTestReconciler(t, sc, foreignGatewaySecret(sc.Name))
	r.Providers = kubeconfigRegistry(tokenCredential(), nil)
	r.probeFn = func(_ context.Context, _ *v1beta1.SpokeCluster) (time.Duration, error) {
		t.Error("probe must not run after a failed registration")
		return 0, nil
	}

	if _, err := reconcileOnce(t, r, sc); err == nil {
		t.Fatal("Reconcile returned nil, want the registration error")
	}

	latest := readSpoke(t, r, sc)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionRegistered, metav1.ConditionFalse, reasonRegisterFailed)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionTrue, reasonMaterialized)
	// Unknown, not Disconnected and not left blank: registration failed before any probe, so
	// reachability was never observed. A blank connection renders as an empty STATUS column
	// and makes a refused registration look like a spoke nobody has looked at yet.
	if latest.Status.Connection != v1beta1.ConnectionStateUnknown {
		t.Errorf("status.connection = %q, want %q after a registration failure", latest.Status.Connection, v1beta1.ConnectionStateUnknown)
	}
	wantCondition(t, latest, v1beta1.SpokeClusterConditionConnected, metav1.ConditionUnknown, reasonRegisterFailed)
})

// TestReconcileDiscoveryFailurePreservesClusterInfo keeps a transient inventory failure
// from erasing the last known good inventory, and from failing the pass.
var _ = It("ReconcileDiscoveryFailurePreservesClusterInfo", func() {
	t := GinkgoT()
	sc := connectableSpoke("spoke-stale-info")
	sc.Status.ClusterInfo = &v1beta1.SpokeClusterInfo{KubernetesVersion: "v1.30.0", NodeCount: 3}
	r := connectedReconciler(t, sc)
	r.discoverFn = func(_ context.Context, _ *v1beta1.SpokeCluster, _ *credential.Materialized, _ time.Duration) (*v1beta1.SpokeClusterInfo, error) {
		return nil, errors.New("nodes is forbidden")
	}

	if _, err := reconcileOnce(t, r, sc); err != nil {
		t.Fatalf("a discovery failure must not fail the pass: %v", err)
	}

	latest := readSpoke(t, r, sc)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionInfoSynced, metav1.ConditionFalse, reasonDiscoveryFailed)
	wantCondition(t, latest, v1beta1.SpokeClusterConditionConnected, metav1.ConditionTrue, reasonProbeSucceeded)
	if latest.Status.ClusterInfo == nil {
		t.Fatal("status.clusterInfo was cleared by a discovery failure, want the previous value preserved")
	}
	if latest.Status.ClusterInfo.KubernetesVersion != "v1.30.0" || latest.Status.ClusterInfo.NodeCount != 3 {
		t.Errorf("status.clusterInfo = %+v, want the previous value preserved", latest.Status.ClusterInfo)
	}
})

// TestReconcileIgnoresMissingSpokeCluster covers the ordinary case of reconciling an object
// that has already been fully deleted.
var _ = It("ReconcileIgnoresMissingSpokeCluster", func() {
	t := GinkgoT()
	r := newTestReconciler(t)
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: "gone", Namespace: "vela-system"},
	})
	if err != nil {
		t.Fatalf("a missing SpokeCluster must not be an error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want no requeue for a missing object", res.RequeueAfter)
	}
})

// TestReconcileDeletionRunsNoConnectWork proves the deletion dispatch happens before any
// external side effect, so a deleting spoke is never re-registered or re-probed.
var _ = It("ReconcileDeletionRunsNoConnectWork", func() {
	t := GinkgoT()
	sc := connectableSpoke("spoke-deleting")
	now := metav1.Now()
	sc.DeletionTimestamp = &now
	sc.Finalizers = []string{FinalizerName}

	r := newTestReconciler(t, sc, gatewaySecret(sc.Name))
	r.Providers = kubeconfigRegistry(tokenCredential(), nil)
	r.probeFn = func(_ context.Context, _ *v1beta1.SpokeCluster) (time.Duration, error) {
		t.Error("probe must not run for a SpokeCluster being deleted")
		return 0, nil
	}
	r.discoverFn = func(_ context.Context, _ *v1beta1.SpokeCluster, _ *credential.Materialized, _ time.Duration) (*v1beta1.SpokeClusterInfo, error) {
		t.Error("discovery must not run for a SpokeCluster being deleted")
		return nil, nil
	}

	if _, err := reconcileOnce(t, r, sc); err != nil {
		t.Fatalf("Reconcile on the deletion path returned an unexpected error: %v", err)
	}
})

// TestNextRequeue is the only coverage of the refresh cap, which is what keeps an AWS spoke
// reconciling before its minted token expires instead of on the plain probe cadence.
var _ = It("NextRequeue", func() {
	t := GinkgoT()
	cases := map[string]struct {
		intervalSeconds int32
		nextRefreshIn   time.Duration
		want            time.Duration
	}{
		"static credential follows the probe interval": {
			intervalSeconds: 30,
			nextRefreshIn:   0, // zero NextRefresh means the credential never expires
			want:            30 * time.Second,
		},
		"non-positive interval falls back to the default": {
			intervalSeconds: 0,
			nextRefreshIn:   0,
			want:            defaultProbeInterval,
		},
		"imminent refresh caps the interval": {
			intervalSeconds: 300,
			nextRefreshIn:   45 * time.Second,
			want:            45 * time.Second,
		},
		"distant refresh leaves the interval alone": {
			intervalSeconds: 30,
			nextRefreshIn:   14 * time.Minute,
			want:            30 * time.Second,
		},
	}

	for name, tc := range cases {
		By(name, func() {
			sc := connectableSpoke("spoke-requeue")
			sc.Spec.ProbeIntervalSeconds = tc.intervalSeconds
			m := tokenCredential()
			if tc.nextRefreshIn != 0 {
				m.NextRefresh = time.Now().Add(tc.nextRefreshIn)
			}

			got := nextRequeue(sc, m)
			// time.Until is evaluated inside nextRequeue, so allow a small execution skew.
			if diff := got - tc.want; diff > time.Second || diff < -time.Second {
				t.Errorf("nextRequeue = %v, want approximately %v", got, tc.want)
			}
		})
	}
})

// TestReconcileRequeueFlooredAtMinimum stops a past-due refresh deadline from turning the
// requeue into a hot loop.
var _ = It("ReconcileRequeueFlooredAtMinimum", func() {
	t := GinkgoT()
	sc := connectableSpoke("spoke-expired-token")
	overdue := tokenCredential()
	overdue.NextRefresh = time.Now().Add(-time.Hour)

	r := connectedReconciler(t, sc)
	r.Providers = kubeconfigRegistry(overdue, nil)

	res, err := reconcileOnce(t, r, sc)
	if err != nil {
		t.Fatalf("Reconcile returned an unexpected error: %v", err)
	}
	if res.RequeueAfter != minRequeue {
		t.Errorf("RequeueAfter = %v, want the %v floor for an overdue refresh deadline", res.RequeueAfter, minRequeue)
	}
})

// TestIgnoreOwnStatusWrites locks the event filter. Without it a healthy spoke reconciled
// roughly four times more often than its probe interval asks for, because each status write
// came back as an update event; with it, cadence comes from RequeueAfter alone. The deletion
// and spec-change cases must still get through.
var _ = It("IgnoreOwnStatusWrites", func() {
	t := GinkgoT()
	deleting := func(sc *v1beta1.SpokeCluster) *v1beta1.SpokeCluster {
		out := sc.DeepCopy()
		now := metav1.Now()
		out.DeletionTimestamp = &now
		return out
	}
	withStatus := func(sc *v1beta1.SpokeCluster) *v1beta1.SpokeCluster {
		out := sc.DeepCopy()
		probed := metav1.Now()
		out.Status.LastProbeTime = &probed
		out.Status.ClusterInfo = &v1beta1.SpokeClusterInfo{LatencyMillis: 3}
		return out
	}
	bumpGeneration := func(sc *v1beta1.SpokeCluster) *v1beta1.SpokeCluster {
		out := sc.DeepCopy()
		out.Generation++
		return out
	}
	addFinalizer := func(sc *v1beta1.SpokeCluster) *v1beta1.SpokeCluster {
		out := sc.DeepCopy()
		out.Finalizers = []string{FinalizerName}
		return out
	}

	base := connectableSpoke("spoke-predicate")
	cases := map[string]struct {
		newObj *v1beta1.SpokeCluster
		want   bool
	}{
		"status-only write is filtered":    {newObj: withStatus(base), want: false},
		"finalizer-only write is filtered": {newObj: addFinalizer(base), want: false},
		"spec change gets through":         {newObj: bumpGeneration(base), want: true},
		"deletion gets through":            {newObj: deleting(base), want: true},
	}

	for name, tc := range cases {
		By(name, func() {
			got := ignoreOwnStatusWrites.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: tc.newObj})
			if got != tc.want {
				t.Errorf("predicate returned %v, want %v", got, tc.want)
			}
		})
	}
})

// TestProbeIntervalFallback locks the guard that keeps working for objects built before the
// schema default landed, and for objects built directly in tests.
var _ = It("ProbeIntervalFallback", func() {
	t := GinkgoT()
	cases := map[string]struct {
		seconds int32
		want    time.Duration
	}{
		"positive value is honoured": {seconds: 60, want: 60 * time.Second},
		"zero falls back":            {seconds: 0, want: defaultProbeInterval},
		"negative falls back":        {seconds: -5, want: defaultProbeInterval},
	}
	for name, tc := range cases {
		By(name, func() {
			sc := connectableSpoke("spoke-interval")
			sc.Spec.ProbeIntervalSeconds = tc.seconds
			if got := probeInterval(sc); got != tc.want {
				t.Errorf("probeInterval = %v, want %v", got, tc.want)
			}
		})
	}
})

// cachingReconciler is connectedReconciler with a real credential cache and a provider
// whose Materialize calls can be counted. A hit and a miss are indistinguishable in status
// and in the gateway Secret by design, so the counter is the only observable difference.
func cachingReconciler(t GinkgoTInterface, sc *v1beta1.SpokeCluster, m *credential.Materialized) (*Reconciler, *mockProvider) {
	t.Helper()
	r := connectedReconciler(t, sc)
	reg, provider := kubeconfigRegistryWithProvider(m, nil)
	r.Providers = reg

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.credentials = newCredentialCache(ctx, DefaultCredentialCacheTTL)
	return r, provider
}

var _ = It("ReconcileReusesTheCachedCredential", func() {
	t := GinkgoT()
	sc := connectableSpoke("cached")
	r, provider := cachingReconciler(t, sc, refreshingCredential(13*time.Minute))

	var messages []string
	for pass := 1; pass <= 3; pass++ {
		if _, err := reconcileOnce(t, r, sc); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		latest := readSpoke(t, r, sc)
		wantCondition(t, latest, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionTrue, reasonMaterialized)
		wantCondition(t, latest, v1beta1.SpokeClusterConditionConnected, metav1.ConditionTrue, reasonProbeSucceeded)
		messages = append(messages, meta.FindStatusCondition(latest.Status.Conditions, v1beta1.SpokeClusterConditionCredentialValid).Message)
	}

	if provider.calls != 1 {
		t.Errorf("Materialize ran %d times across three passes, want 1", provider.calls)
	}
	// The message is asserted, not just the reason: a cached pass must be invisible to an
	// operator reading status, and the message is the part carrying the endpoint.
	for i, msg := range messages {
		if msg != messages[0] {
			t.Errorf("pass %d message = %q, want %q on every pass", i+1, msg, messages[0])
		}
	}
})

var _ = It("ReconcileWritesIdenticalSecretBytesOnACachedPass", func() {
	t := GinkgoT()
	sc := connectableSpoke("stable-bytes")
	r, _ := cachingReconciler(t, sc, refreshingCredential(13*time.Minute))

	if _, err := reconcileOnce(t, r, sc); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first := readGatewaySecret(t, r.Client, sc.Name)

	if _, err := reconcileOnce(t, r, sc); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	second := readGatewaySecret(t, r.Client, sc.Name)

	// Identical bytes are what make the real apiserver skip the etcd write. The fake client
	// bumps resourceVersion on every Update regardless, so the bytes are the assertable part.
	for _, key := range []string{secretKeyToken, secretKeyCACert, secretKeyEndpoint} {
		if string(first.Data[key]) != string(second.Data[key]) {
			t.Errorf("secret key %q changed between a fresh pass and a cached one", key)
		}
	}
})

var _ = It("ReconcileDoesNotCacheCredentialsWithoutARefreshDeadline", func() {
	t := GinkgoT()
	sc := connectableSpoke("static-cred")
	// tokenCredential has a zero NextRefresh, which is what every kubeconfig spoke reports.
	r, provider := cachingReconciler(t, sc, tokenCredential())

	for pass := 1; pass <= 2; pass++ {
		if _, err := reconcileOnce(t, r, sc); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	if provider.calls != 2 {
		t.Errorf("Materialize ran %d times, want 2: a credential with no refresh deadline must be re-read every pass so a rotated source Secret is picked up", provider.calls)
	}
	if _, hit := r.credentials.Get(readSpoke(t, r, sc), 0); hit {
		t.Errorf("a credential with no refresh deadline must not be stored")
	}
})

var _ = It("ReconcileWithoutACredentialCacheRematerializes", func() {
	t := GinkgoT()
	sc := connectableSpoke("no-cache")
	r := connectedReconciler(t, sc)
	reg, provider := kubeconfigRegistryWithProvider(refreshingCredential(13*time.Minute), nil)
	r.Providers = reg
	// r.credentials stays nil, as it does for every Reconciler built directly.

	for pass := 1; pass <= 2; pass++ {
		if _, err := reconcileOnce(t, r, sc); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}

	if provider.calls != 2 {
		t.Errorf("Materialize ran %d times, want 2: a nil cache means caching is off", provider.calls)
	}
})

var _ = It("ReconcileRematerializesAfterASpecChange", func() {
	t := GinkgoT()
	sc := connectableSpoke("spec-change")
	r, provider := cachingReconciler(t, sc, refreshingCredential(13*time.Minute))

	if _, err := reconcileOnce(t, r, sc); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	latest := readSpoke(t, r, sc)
	latest.Generation++
	if err := r.Update(context.Background(), latest); err != nil {
		t.Fatalf("failed to bump generation: %v", err)
	}

	if _, err := reconcileOnce(t, r, sc); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if provider.calls != 2 {
		t.Errorf("Materialize ran %d times, want 2: a spec change must not reuse the old credential", provider.calls)
	}
})

var _ = It("ReconcileEvictsTheCachedCredentialOnlyOn401", func() {
	t := GinkgoT()
	for _, tc := range []struct {
		name      string
		probeErr  error
		wantCalls int
	}{
		{
			name:      "401 evicts, so the next pass remints",
			probeErr:  apierrors.NewUnauthorized("the spoke rejected the credential"),
			wantCalls: 2,
		},
		{
			name:      "403 does not evict, because reminting cannot fix RBAC",
			probeErr:  apierrors.NewForbidden(schema.GroupResource{Resource: "healthz"}, "healthz", errors.New("forbidden")),
			wantCalls: 1,
		},
		{
			name:      "a timeout does not evict, because the credential was never tested",
			probeErr:  context.DeadlineExceeded,
			wantCalls: 1,
		},
	} {
		By(tc.name, func() {
			sc := connectableSpoke("probe-fail")
			r, provider := cachingReconciler(t, sc, refreshingCredential(13*time.Minute))
			r.probeFn = func(_ context.Context, _ *v1beta1.SpokeCluster) (time.Duration, error) {
				return 0, tc.probeErr
			}

			for pass := 1; pass <= 2; pass++ {
				if _, err := reconcileOnce(t, r, sc); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
			}

			if provider.calls != tc.wantCalls {
				t.Errorf("Materialize ran %d times, want %d", provider.calls, tc.wantCalls)
			}
			// Whatever the eviction did, status reads the same: the spoke is unreachable.
			latest := readSpoke(t, r, sc)
			wantCondition(t, latest, v1beta1.SpokeClusterConditionConnected, metav1.ConditionFalse, reasonProbeFailed)
			if latest.Status.Connection != v1beta1.ConnectionStateDisconnected {
				t.Errorf("connection = %q, want %q", latest.Status.Connection, v1beta1.ConnectionStateDisconnected)
			}
		})
	}
})

var _ = It("StatusNeedsWriteIgnoresHeartbeatFields", func() {
	t := GinkgoT()
	base := v1beta1.SpokeClusterStatus{
		ObservedGeneration: 1,
		Connection:         v1beta1.ConnectionStateConnected,
		LastProbeTime:      &metav1.Time{Time: time.Unix(100, 0)},
		ClusterInfo: &v1beta1.SpokeClusterInfo{
			KubernetesVersion: "v1.31.5+k3s1",
			NodeCount:         1,
			LatencyMillis:     4,
			LastSyncedTime:    &metav1.Time{Time: time.Unix(100, 0)},
		},
	}
	heartbeat := base.DeepCopy()
	heartbeat.LastProbeTime = &metav1.Time{Time: time.Unix(130, 0)}
	heartbeat.ClusterInfo.LatencyMillis = 9
	heartbeat.ClusterInfo.LastSyncedTime = &metav1.Time{Time: time.Unix(130, 0)}
	if statusNeedsWrite(base, *heartbeat) {
		t.Fatal("heartbeat-only status change should not write")
	}

	versionBump := base.DeepCopy()
	versionBump.ClusterInfo.KubernetesVersion = "v1.32.0"
	if !statusNeedsWrite(base, *versionBump) {
		t.Fatal("inventory change must write")
	}

	disconnected := base.DeepCopy()
	disconnected.Connection = v1beta1.ConnectionStateDisconnected
	if !statusNeedsWrite(base, *disconnected) {
		t.Fatal("connection change must write")
	}

	empty := v1beta1.SpokeClusterStatus{}
	if !statusNeedsWrite(empty, base) {
		t.Fatal("first status populate must write")
	}
})

var _ = It("ReconcileSkipsHeartbeatOnlyStatusWrite", func() {
	t := GinkgoT()
	sc := connectableSpoke("spoke-heartbeat")
	r := connectedReconciler(t, sc)

	if _, err := reconcileOnce(t, r, sc); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first := readSpoke(t, r, sc)
	if first.Status.LastProbeTime == nil {
		t.Fatal("first pass must record lastProbeTime")
	}
	firstProbe := *first.Status.LastProbeTime
	firstRV := first.ResourceVersion

	if _, err := reconcileOnce(t, r, sc); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	second := readSpoke(t, r, sc)
	if !second.Status.LastProbeTime.Equal(&firstProbe) {
		t.Errorf("lastProbeTime moved on a heartbeat-only pass: %v -> %v", firstProbe, second.Status.LastProbeTime)
	}
	if second.ResourceVersion != firstRV {
		t.Errorf("resourceVersion moved on a heartbeat-only pass: %s -> %s", firstRV, second.ResourceVersion)
	}
})

var _ = It("KubeconfigSecretIndexKeys", func() {
	t := GinkgoT()
	sc := connectableSpoke("idx")
	got := kubeconfigSecretIndexKeys(sc)
	want := sc.Namespace + "/" + sc.Spec.Credential.Kubeconfig.SecretRef.Name
	if len(got) != 1 || got[0] != want {
		t.Fatalf("keys = %v, want [%s]", got, want)
	}

	sc.Spec.Credential.Kubeconfig.SecretRef.Namespace = ""
	got = kubeconfigSecretIndexKeys(sc)
	if len(got) != 1 || got[0] != sc.Namespace+"/"+sc.Spec.Credential.Kubeconfig.SecretRef.Name {
		t.Fatalf("empty namespace should fall back to spoke ns, got %v", got)
	}

	sc.Spec.Credential.Type = v1beta1.CredentialTypeAWS
	if keys := kubeconfigSecretIndexKeys(sc); keys != nil {
		t.Fatalf("aws spoke should not index a kubeconfig secret, got %v", keys)
	}
})

var _ = It("MapKubeconfigSecretEnqueuesOwner", func() {
	t := GinkgoT()
	sc := connectableSpoke("map-owner")
	other := connectableSpoke("map-other")
	r := newTestReconciler(t, sc, other)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      sc.Spec.Credential.Kubeconfig.SecretRef.Name,
		Namespace: sc.Namespace,
	}}
	reqs := r.mapKubeconfigSecret(context.Background(), secret)
	if len(reqs) != 1 || reqs[0].Name != sc.Name || reqs[0].Namespace != sc.Namespace {
		t.Fatalf("requests = %v, want only %s/%s", reqs, sc.Namespace, sc.Name)
	}

	// Empty secretRef.namespace is what GitOps usually writes; it must still match.
	sc.Spec.Credential.Kubeconfig.SecretRef.Namespace = ""
	if err := r.Update(context.Background(), sc); err != nil {
		t.Fatalf("clear secretRef.namespace: %v", err)
	}
	reqs = r.mapKubeconfigSecret(context.Background(), secret)
	if len(reqs) != 1 || reqs[0].Name != sc.Name {
		t.Fatalf("empty secretRef.namespace: requests = %v, want %s", reqs, sc.Name)
	}
})

var _ = It("SourceKubeconfigSecretPredicateIgnoresGatewaySecrets", func() {
	t := GinkgoT()
	src := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "src", Namespace: "vela-system"}}
	if !sourceKubeconfigSecretPredicate.Create(event.CreateEvent{Object: src}) {
		t.Fatal("source kubeconfig Secret should enqueue")
	}

	owned := src.DeepCopy()
	owned.Annotations = map[string]string{secretOwnerAnnotation: "vela-system/spoke"}
	if sourceKubeconfigSecretPredicate.Create(event.CreateEvent{Object: owned}) {
		t.Fatal("owner-annotated gateway Secret should not enqueue")
	}

	labeled := src.DeepCopy()
	labeled.Labels = map[string]string{clustercommon.LabelKeyClusterCredentialType: "ServiceAccountToken"}
	if sourceKubeconfigSecretPredicate.Update(event.UpdateEvent{ObjectNew: labeled}) {
		t.Fatal("credential-type labeled gateway Secret should not enqueue")
	}
})
