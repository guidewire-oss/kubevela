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

	. "github.com/onsi/ginkgo/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

// secretGetMutator wraps Get so tests can simulate a Secret that changed between
// List and the janitor's re-read (new UID, new owner, or a bumped resourceVersion).
type secretGetMutator struct {
	client.Client
	mutate func(*corev1.Secret)
}

func (c secretGetMutator) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if c.mutate == nil {
		return nil
	}
	if secret, ok := obj.(*corev1.Secret); ok {
		c.mutate(secret)
	}
	return nil
}

var _ = It("ParseSecretOwner", func() {
	t := GinkgoT()
	ns, name, ok := parseSecretOwner("team-a/spoke")
	if !ok || ns != "team-a" || name != "spoke" {
		t.Fatalf("parseSecretOwner(team-a/spoke) = %q %q %v", ns, name, ok)
	}
	for _, bad := range []string{"", "noslash", "/onlyname", "onlyns/", "a/b/c"} {
		if _, _, ok := parseSecretOwner(bad); ok {
			t.Fatalf("parseSecretOwner(%q) = ok, want false", bad)
		}
	}
})

var _ = It("JanitorReapsForceDeletedCrossNamespaceDetach", func() {
	t := GinkgoT()
	// Simulate the leak: SpokeCluster in team-a is gone, gateway Secret remains with
	// owner annotation and detach policy (no OwnerReference possible cross-ns).
	secret := gatewaySecretOwnedBy("leaked-spoke", "team-a")
	secret.Annotations[secretDeletionPolicyAnnotation] = string(v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, secret)

	r.sweepOrphanedGatewaySecrets(context.Background())

	if secretExists(t, r.Client, "leaked-spoke") {
		t.Fatal("janitor left the leaked gateway Secret in place, want it reaped")
	}
})

var _ = It("JanitorScrubsResourceTrackersAfterReap", func() {
	t := GinkgoT()
	secret := gatewaySecretOwnedBy("rt-spoke", "team-a")
	secret.Annotations[secretDeletionPolicyAnnotation] = string(v1beta1.SpokeDeletionPolicyDetach)
	rt := &v1beta1.ResourceTracker{
		ObjectMeta: metav1.ObjectMeta{Name: "app-rt-spoke"},
		Spec: v1beta1.ResourceTrackerSpec{
			ManagedResources: []v1beta1.ManagedResource{
				{ClusterObjectReference: common.ClusterObjectReference{Cluster: "rt-spoke"}},
				{ClusterObjectReference: common.ClusterObjectReference{Cluster: "other"}},
			},
		},
	}
	r := newTestReconciler(t, secret, rt)

	r.sweepOrphanedGatewaySecrets(context.Background())

	if secretExists(t, r.Client, "rt-spoke") {
		t.Fatal("janitor left the leaked gateway Secret in place, want it reaped")
	}
	got := &v1beta1.ResourceTracker{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "app-rt-spoke"}, got); err != nil {
		t.Fatalf("get ResourceTracker: %v", err)
	}
	if len(got.Spec.ManagedResources) != 1 || got.Spec.ManagedResources[0].Cluster != "other" {
		t.Fatalf("ManagedResources = %+v, want only cluster other", got.Spec.ManagedResources)
	}
})

var _ = It("JanitorKeepsOrphanPolicySecret", func() {
	t := GinkgoT()
	secret := gatewaySecretOwnedBy("orphaned-spoke", "team-a")
	secret.Annotations[secretDeletionPolicyAnnotation] = string(v1beta1.SpokeDeletionPolicyOrphan)
	r := newTestReconciler(t, secret)

	r.sweepOrphanedGatewaySecrets(context.Background())

	if !secretExists(t, r.Client, "orphaned-spoke") {
		t.Fatal("janitor deleted an orphan-policy Secret, want it kept")
	}
})

var _ = It("JanitorKeepsSecretWhenSpokeClusterExists", func() {
	t := GinkgoT()
	sc := spokeIn("live-spoke", "team-a", v1beta1.SpokeDeletionPolicyDetach)
	secret := gatewaySecretOwnedBy("live-spoke", "team-a")
	secret.Annotations[secretDeletionPolicyAnnotation] = string(v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc, secret)

	r.sweepOrphanedGatewaySecrets(context.Background())

	if !secretExists(t, r.Client, "live-spoke") {
		t.Fatal("janitor deleted a Secret whose SpokeCluster still exists")
	}
})

var _ = It("JanitorIgnoresManuallyJoinedSecret", func() {
	t := GinkgoT()
	secret := foreignGatewaySecret("manual-join")
	r := newTestReconciler(t, secret)

	r.sweepOrphanedGatewaySecrets(context.Background())

	if !secretExists(t, r.Client, "manual-join") {
		t.Fatal("janitor deleted a manually joined Secret without owner annotation")
	}
})

var _ = It("JanitorSkipsSecretReplacedBetweenListAndReap", func() {
	t := GinkgoT()
	secret := gatewaySecretOwnedBy("replaced-spoke", "team-a")
	r := newTestReconciler(t, secret)
	r.Client = secretGetMutator{
		Client: r.Client,
		mutate: func(s *corev1.Secret) {
			s.UID = types.UID("uid-after-replace")
			s.Annotations[secretOwnerAnnotation] = "other-ns/other"
		},
	}

	r.sweepOrphanedGatewaySecrets(context.Background())

	if !secretExists(t, r.Client.(secretGetMutator).Client, "replaced-spoke") {
		t.Fatal("janitor detached a Secret whose UID/owner changed after list")
	}
})

var _ = It("JanitorSkipsSecretWhenResourceVersionMoved", func() {
	t := GinkgoT()
	secret := gatewaySecretOwnedBy("rv-spoke", "team-a")
	r := newTestReconciler(t, secret)
	r.Client = secretGetMutator{
		Client: r.Client,
		mutate: func(s *corev1.Secret) {
			s.ResourceVersion = s.ResourceVersion + "-stale"
		},
	}

	r.sweepOrphanedGatewaySecrets(context.Background())

	if !secretExists(t, r.Client.(secretGetMutator).Client, "rv-spoke") {
		t.Fatal("janitor detached a Secret whose resourceVersion changed after list")
	}
})

var _ = It("JanitorTreatsMissingDeletionPolicyAsDetach", func() {
	t := GinkgoT()
	// Secrets written before the deletion-policy annotation existed should still be
	// reaped when their SpokeCluster is gone (API default is detach).
	secret := gatewaySecretOwnedBy("legacy-spoke", "team-a")
	r := newTestReconciler(t, secret)

	r.sweepOrphanedGatewaySecrets(context.Background())

	if secretExists(t, r.Client, "legacy-spoke") {
		t.Fatal("janitor left a legacy detach Secret whose SpokeCluster is gone")
	}
})

var _ = It("RegisterStampsDeletionPolicyAnnotation", func() {
	t := GinkgoT()
	sc := spokeIn("spoke", "team-a", v1beta1.SpokeDeletionPolicyOrphan)
	r := newTestReconciler(t, sc)
	if err := r.register(context.Background(), sc, tokenCredential()); err != nil {
		t.Fatalf("register: %v", err)
	}
	secret := readGatewaySecret(t, r.Client, sc.Name)
	if got := secret.Annotations[secretDeletionPolicyAnnotation]; got != string(v1beta1.SpokeDeletionPolicyOrphan) {
		t.Fatalf("deletion-policy annotation = %q, want orphan", got)
	}
})

var _ = It("RegisterStampsDetachWhenPolicyUnset", func() {
	t := GinkgoT()
	sc := spokeIn("spoke", "team-a", "")
	r := newTestReconciler(t, sc)
	if err := r.register(context.Background(), sc, tokenCredential()); err != nil {
		t.Fatalf("register: %v", err)
	}
	secret := readGatewaySecret(t, r.Client, sc.Name)
	if got := secret.Annotations[secretDeletionPolicyAnnotation]; got != string(v1beta1.SpokeDeletionPolicyDetach) {
		t.Fatalf("deletion-policy annotation = %q, want detach", got)
	}
})
