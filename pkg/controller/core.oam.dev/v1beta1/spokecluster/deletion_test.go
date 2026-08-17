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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/multicluster"
)

// gatewaySecret builds a gateway Secret standing in for one this spoke registered itself:
// the credential-type label is load-bearing (DetachCluster reads it through
// getMutableClusterSecret, which refuses to touch an unlabelled Secret), and the owner
// annotation is what reconcileDelete's ownership gate requires before it will touch the
// Secret at all.
func gatewaySecret(name string) *corev1.Secret {
	return gatewaySecretOwnedBy(name, multicluster.ClusterGatewaySecretNamespace)
}

// gatewaySecretOwnedBy builds a gateway Secret stamped for a SpokeCluster in ownerNamespace.
// The namespace matters because verifyAdoptable matches the annotation against the
// SpokeCluster's own namespace/name, so a spoke outside the gateway namespace only
// recognizes a Secret stamped with its own namespace.
func gatewaySecretOwnedBy(name, ownerNamespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   multicluster.ClusterGatewaySecretNamespace,
			Labels:      map[string]string{credentialTypeLabel: "ServiceAccountToken"},
			Annotations: map[string]string{secretOwnerAnnotation: ownerNamespace + "/" + name},
		},
		Data: map[string][]byte{"endpoint": []byte("https://spoke.example.com"), "token": []byte("tok")},
	}
}

// foreignGatewaySecret builds a gateway Secret this SpokeCluster never registered: no
// owner annotation, standing in for a manually joined cluster or another SpokeCluster's
// registration that merely shares a name.
func foreignGatewaySecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: multicluster.ClusterGatewaySecretNamespace,
			Labels:    map[string]string{credentialTypeLabel: "ServiceAccountToken"},
		},
		Data: map[string][]byte{"endpoint": []byte("https://other-cluster.example.com"), "token": []byte("original-tok")},
	}
}

// deletingSpoke builds a SpokeCluster mid-deletion, carrying the finalizer that holds it.
// A fake client needs the finalizer present for an object with a deletion timestamp.
func deletingSpoke(name string, policy v1beta1.SpokeDeletionPolicy) *v1beta1.SpokeCluster {
	return deletingSpokeIn(name, multicluster.ClusterGatewaySecretNamespace, policy)
}

func deletingSpokeIn(name, namespace string, policy v1beta1.SpokeDeletionPolicy) *v1beta1.SpokeCluster {
	sc := spokeIn(name, namespace, policy)
	now := metav1.Now()
	sc.DeletionTimestamp = &now
	sc.Finalizers = []string{FinalizerName}
	return sc
}

func secretExists(t GinkgoTInterface, cli client.Client, name string) bool {
	t.Helper()
	key := gatewayKey(name)
	err := cli.Get(context.Background(), key, &corev1.Secret{})
	switch {
	case err == nil:
		return true
	case apierrors.IsNotFound(err):
		return false
	default:
		t.Fatalf("failed to read gateway secret %s: %v", key, err)
		return false
	}
}

var _ = It("ReconcileDeleteDetachRemovesSecret", func() {
	t := GinkgoT()
	sc := deletingSpoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc, gatewaySecret(sc.Name))

	if _, err := r.reconcileDelete(context.Background(), sc); err != nil {
		t.Fatalf("reconcileDelete returned an unexpected error: %v", err)
	}

	if secretExists(t, r.Client, sc.Name) {
		t.Error("gateway secret survived a detach deletion, want it removed")
	}
	if containsFinalizer(sc) {
		t.Error("finalizer was not released, the SpokeCluster stays wedged")
	}
})

// A detach spoke outside the gateway namespace never gets the owner-reference backstop,
// because owner references cannot cross namespaces. The finalizer is what actually cleans
// up, and it is namespace-independent, so deletion must still remove the Secret. This is
// the compensating mechanism for the skipped reference in
// TestRegisterOutsideGatewayNamespaceSkipsOwnerRef; without it the skip would leak.
var _ = It("ReconcileDeleteDetachRemovesSecretOutsideGatewayNamespace", func() {
	t := GinkgoT()
	sc := deletingSpokeIn("spoke", "team-a", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc, gatewaySecretOwnedBy(sc.Name, sc.Namespace))

	if _, err := r.reconcileDelete(context.Background(), sc); err != nil {
		t.Fatalf("reconcileDelete returned an unexpected error: %v", err)
	}

	if secretExists(t, r.Client, sc.Name) {
		t.Error("gateway secret survived deletion of a spoke outside the gateway namespace, want it removed")
	}
	if containsFinalizer(sc) {
		t.Error("finalizer was not released, the SpokeCluster stays wedged")
	}
})

// Requirement 7.1: the series belong to the object, so every deletion path drops them.
// Orphan and never-registered deletions are the interesting cases: neither is a detach,
// and both would otherwise leave a gauge reporting a spoke that no longer exists.
var _ = It("ReconcileDeleteForgetsMetrics", func() {
	t := GinkgoT()
	cases := []struct {
		name    string
		policy  v1beta1.SpokeDeletionPolicy
		objects []client.Object
	}{
		{name: "detach", policy: v1beta1.SpokeDeletionPolicyDetach},
		{name: "orphan", policy: v1beta1.SpokeDeletionPolicyOrphan},
		{name: "never registered", policy: v1beta1.SpokeDeletionPolicyDetach, objects: []client.Object{}},
	}
	for _, tc := range cases {
		By(tc.name, func() {
			resetSpokeMetrics()
			sc := deletingSpoke("spoke", tc.policy)
			objects := []client.Object{sc}
			if tc.objects == nil {
				objects = append(objects, gatewaySecret(sc.Name))
			}
			r := newTestReconciler(t, objects...)

			observeConnection(sc, &v1beta1.SpokeClusterStatus{Connection: v1beta1.ConnectionStateConnected})
			observeInventory(sc, &v1beta1.SpokeClusterInfo{NodeCount: 2})
			Expect(testutil.CollectAndCount(spokeConnected)).To(Equal(1))

			if _, err := r.reconcileDelete(context.Background(), sc); err != nil {
				t.Fatalf("reconcileDelete returned an unexpected error: %v", err)
			}

			Expect(testutil.CollectAndCount(spokeConnected)).To(BeZero(),
				"a deleted spoke must not keep reporting a connection state")
			Expect(testutil.CollectAndCount(spokeNodeCount)).To(BeZero(),
				"a deleted spoke must not keep reporting an inventory")
		})
	}
})

// An unset policy has to behave as detach, so objects created before schema defaulting
// still clean up after themselves.
var _ = It("ReconcileDeleteUnsetPolicyDetaches", func() {
	t := GinkgoT()
	sc := deletingSpoke("spoke", "")
	r := newTestReconciler(t, sc, gatewaySecret(sc.Name))

	if _, err := r.reconcileDelete(context.Background(), sc); err != nil {
		t.Fatalf("reconcileDelete returned an unexpected error: %v", err)
	}

	if secretExists(t, r.Client, sc.Name) {
		t.Error("gateway secret survived an unset-policy deletion, want detach behaviour")
	}
	if containsFinalizer(sc) {
		t.Error("finalizer was not released")
	}
})

var _ = It("ReconcileDeleteOrphanKeepsSecret", func() {
	t := GinkgoT()
	sc := deletingSpoke("spoke", v1beta1.SpokeDeletionPolicyOrphan)
	rec := &recordingRecorder{}
	r := newTestReconciler(t, sc, gatewaySecret(sc.Name))
	r.record = rec

	if _, err := r.reconcileDelete(context.Background(), sc); err != nil {
		t.Fatalf("reconcileDelete returned an unexpected error: %v", err)
	}

	if !secretExists(t, r.Client, sc.Name) {
		t.Error("gateway secret was removed under the orphan policy, want it kept")
	}
	if containsFinalizer(sc) {
		t.Error("finalizer was not released")
	}
	if len(rec.events) != 0 {
		t.Errorf("orphan deletion emitted %#v, want no Detached event", rec.events)
	}
})

var _ = It("ReconcileDeleteDetachEmitsDetached", func() {
	t := GinkgoT()
	sc := deletingSpoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	rec := &recordingRecorder{}
	r := newTestReconciler(t, sc, gatewaySecret(sc.Name))
	r.record = rec

	if _, err := r.reconcileDelete(context.Background(), sc); err != nil {
		t.Fatalf("reconcileDelete returned an unexpected error: %v", err)
	}
	if len(rec.events) != 1 || rec.events[0].Reason != reasonDetached {
		t.Fatalf("detach deletion events = %#v, want one Detached", rec.events)
	}
})

// A spoke that never finished registering has no gateway Secret, so DetachCluster errors.
// Deletion still has to complete through the direct-delete fallback, otherwise a
// half-registered spoke can never be removed.
var _ = It("ReconcileDeleteNeverRegisteredStillCompletes", func() {
	t := GinkgoT()
	sc := deletingSpoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)

	if _, err := r.reconcileDelete(context.Background(), sc); err != nil {
		t.Fatalf("reconcileDelete returned an unexpected error: %v", err)
	}

	if containsFinalizer(sc) {
		t.Error("finalizer was not released for a never-registered spoke, deletion is wedged")
	}
})

var _ = It("ReconcileDeleteWithoutFinalizerIsNoOp", func() {
	t := GinkgoT()
	sc := deletingSpoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	sc.Finalizers = nil
	r := newTestReconciler(t, gatewaySecret(sc.Name))

	if _, err := r.reconcileDelete(context.Background(), sc); err != nil {
		t.Fatalf("reconcileDelete returned an unexpected error: %v", err)
	}

	if !secretExists(t, r.Client, sc.Name) {
		t.Error("gateway secret was removed without the finalizer present, want no cleanup at all")
	}
})

// A SpokeCluster whose name collides with something it never registered (a manually
// joined cluster, or another SpokeCluster across namespaces) must release its own
// finalizer without touching that foreign Secret at all when it is deleted.
var _ = It("ReconcileDeleteSkipsCleanupForUnownedSecret", func() {
	t := GinkgoT()
	sc := deletingSpoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	foreign := foreignGatewaySecret(sc.Name)
	r := newTestReconciler(t, sc, foreign)

	if _, err := r.reconcileDelete(context.Background(), sc); err != nil {
		t.Fatalf("reconcileDelete returned an unexpected error: %v", err)
	}

	secret := readGatewaySecret(t, r.Client, sc.Name)
	if got := string(secret.Data["token"]); got != "original-tok" {
		t.Errorf("data[token] = %q, the foreign secret was touched despite not being owned", got)
	}
	if containsFinalizer(sc) {
		t.Error("finalizer was not released for the deleting SpokeCluster")
	}
})

var _ = It("ReconcileDeleteUnownedDoesNotEmitDetached", func() {
	t := GinkgoT()
	sc := deletingSpoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	rec := &recordingRecorder{}
	r := newTestReconciler(t, sc, foreignGatewaySecret(sc.Name))
	r.record = rec

	if _, err := r.reconcileDelete(context.Background(), sc); err != nil {
		t.Fatalf("reconcileDelete returned an unexpected error: %v", err)
	}
	if len(rec.events) != 0 {
		t.Errorf("unowned deletion emitted %#v, want no Detached event", rec.events)
	}
})

var _ = It("IsExpectedDetachFailure", func() {
	t := GinkgoT()
	cases := map[string]struct {
		err  error
		want bool
	}{
		"never registered": {
			err:  apierrors.NewNotFound(schema.GroupResource{Group: "cluster.core.oam.dev", Resource: "virtualclusters"}, "spoke"),
			want: true,
		},
		"cluster not exists sentinel": {
			err:  multicluster.ErrClusterNotExists,
			want: true,
		},
		"reserved local name": {
			err:  multicluster.ErrReservedLocalClusterName,
			want: true,
		},
		"resourcetracker scrub failure must retry": {
			err:  fmt.Errorf("error in removing cluster references from resourcetrackers: %w", errors.New("etcdserver: request timed out")),
			want: false,
		},
		"arbitrary API failure must retry": {
			err:  errors.New("connection refused"),
			want: false,
		},
	}
	for name, tc := range cases {
		By(name, func() {
			if got := isExpectedDetachFailure(tc.err); got != tc.want {
				t.Errorf("isExpectedDetachFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
})

func containsFinalizer(sc *v1beta1.SpokeCluster) bool {
	for _, f := range sc.Finalizers {
		if f == FinalizerName {
			return true
		}
	}
	return false
}
