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
	"strings"

	clustercommon "github.com/oam-dev/cluster-gateway/pkg/common"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/multicluster"
	"github.com/oam-dev/kubevela/pkg/spokecluster/credential"
)

// credentialTypeLabel is the label cluster-gateway keys on to recognize a Secret as a
// cluster. It is a var in cluster-gateway, not a const, so it cannot be aliased as one.
var credentialTypeLabel = clustercommon.LabelKeyClusterCredentialType

// testScheme carries the core types plus the vela API group so SetControllerReference can
// resolve the SpokeCluster GVK.
func testScheme(t GinkgoTInterface) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 to scheme: %v", err)
	}
	if err := v1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add v1beta1 to scheme: %v", err)
	}
	return scheme
}

// newTestReconciler builds a Reconciler over a fake client seeded with objs. The status
// subresource is registered because the reconcile loop writes status through it; without
// that the fake client would fold status writes into the main object and hide bugs.
func newTestReconciler(t GinkgoTInterface, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := testScheme(t)
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&v1beta1.SpokeCluster{}).
		Build()
	// Deliberately a second, distinct client. Spoke reads must go through SpokeReader, so
	// any code that reaches for the hub client instead fails identity checks rather than
	// quietly passing because both names point at the same object.
	spokeReader := fake.NewClientBuilder().WithScheme(scheme).Build()
	return &Reconciler{Client: cli, SpokeReader: spokeReader, Scheme: scheme}
}

// spoke builds a SpokeCluster in the gateway namespace, where a detach ownerRef on the
// gateway Secret is legal (owner and dependent must share a namespace).
func spoke(name string, policy v1beta1.SpokeDeletionPolicy) *v1beta1.SpokeCluster {
	return spokeIn(name, multicluster.ClusterGatewaySecretNamespace, policy)
}

func spokeIn(name, namespace string, policy v1beta1.SpokeDeletionPolicy) *v1beta1.SpokeCluster {
	return &v1beta1.SpokeCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       apitypes.UID("uid-" + name),
		},
		Spec: v1beta1.SpokeClusterSpec{DeletionPolicy: policy},
	}
}

func tokenCredential() *credential.Materialized {
	return &credential.Materialized{
		Endpoint: "https://spoke.example.com",
		CAData:   []byte("ca-pem"),
		Token:    "tok",
	}
}

func certCredential() *credential.Materialized {
	return &credential.Materialized{
		Endpoint:       "https://spoke.example.com",
		CAData:         []byte("ca-pem"),
		ClientCertData: []byte("cert-pem"),
		ClientKeyData:  []byte("key-pem"),
	}
}

// gatewayKey is where a spoke's gateway Secret lives, from a bare cluster name.
func gatewayKey(name string) apitypes.NamespacedName {
	return apitypes.NamespacedName{Name: name, Namespace: multicluster.ClusterGatewaySecretNamespace}
}

// readGatewaySecret fetches the gateway Secret for name, failing the test if absent.
func readGatewaySecret(t GinkgoTInterface, cli client.Client, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	key := gatewayKey(name)
	if err := cli.Get(context.Background(), key, secret); err != nil {
		t.Fatalf("failed to read gateway secret %s: %v", key, err)
	}
	return secret
}

var _ = It("RegisterSecretShape", func() {
	t := GinkgoT()
	cases := map[string]struct {
		materialized *credential.Materialized
		wantData     map[string]string
		wantAbsent   []string
		wantCredType string
	}{
		"token credential writes endpoint, ca and token": {
			materialized: tokenCredential(),
			wantData: map[string]string{
				"endpoint": "https://spoke.example.com",
				"ca.crt":   "ca-pem",
				"token":    "tok",
			},
			wantAbsent:   []string{"tls.crt", "tls.key"},
			wantCredType: "ServiceAccountToken",
		},
		"client cert credential writes the tls pair": {
			materialized: certCredential(),
			wantData: map[string]string{
				"endpoint": "https://spoke.example.com",
				"ca.crt":   "ca-pem",
				"tls.crt":  "cert-pem",
				"tls.key":  "key-pem",
			},
			wantAbsent:   []string{"token"},
			wantCredType: "X509Certificate",
		},
		"proxy-url is written when set": {
			materialized: &credential.Materialized{
				Endpoint: "https://spoke.example.com",
				CAData:   []byte("ca-pem"),
				Token:    "tok",
				ProxyURL: "http://10.0.0.1:8080",
			},
			wantData: map[string]string{
				"endpoint":  "https://spoke.example.com",
				"ca.crt":    "ca-pem",
				"token":     "tok",
				"proxy-url": "http://10.0.0.1:8080",
			},
			wantCredType: "ServiceAccountToken",
		},
	}

	for name, tc := range cases {
		By(name, func() {
			sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
			r := newTestReconciler(t, sc)

			if _, err := r.register(context.Background(), sc, tc.materialized); err != nil {
				t.Fatalf("register returned an unexpected error: %v", err)
			}

			secret := readGatewaySecret(t, r.Client, sc.Name)
			if secret.Type != corev1.SecretTypeOpaque {
				t.Errorf("secret type = %q, want %q", secret.Type, corev1.SecretTypeOpaque)
			}
			for key, want := range tc.wantData {
				if got := string(secret.Data[key]); got != want {
					t.Errorf("data[%q] = %q, want %q", key, got, want)
				}
			}
			for _, key := range tc.wantAbsent {
				if _, ok := secret.Data[key]; ok {
					t.Errorf("data[%q] is present, want absent", key)
				}
			}
			if got := secret.Labels[credentialTypeLabel]; got != tc.wantCredType {
				t.Errorf("label %s = %q, want %q", credentialTypeLabel, got, tc.wantCredType)
			}
		})
	}
})

var _ = It("RegisterRefusesEmptyCAData", func() {
	t := GinkgoT()
	sc := spoke("no-ca", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)
	_, err := r.register(context.Background(), sc, &credential.Materialized{
		Endpoint: "https://spoke.example.com",
		Token:    "tok",
	})
	if err == nil {
		t.Fatal("register must refuse an empty CA bundle")
	}
	if !strings.Contains(err.Error(), "without a CA bundle") {
		t.Fatalf("error = %v, want it to name the missing CA bundle", err)
	}
	sec := &corev1.Secret{}
	if getErr := r.Get(context.Background(), gatewaySecretKey(sc), sec); getErr == nil {
		t.Fatal("gateway Secret must not be created when CAData is empty")
	}
})

var _ = It("RegisterRefusesReservedLocalName", func() {
	t := GinkgoT()
	sc := spoke(multicluster.ClusterLocalName, v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)
	_, err := r.register(context.Background(), sc, tokenCredential())
	if err == nil {
		t.Fatal("register must refuse the reserved local cluster name")
	}
	if !strings.Contains(err.Error(), multicluster.ClusterLocalName) {
		t.Fatalf("error = %v, want it to name the reserved cluster", err)
	}
})

var _ = It("RegisterOwnershipPerPolicy", func() {
	t := GinkgoT()
	cases := map[string]struct {
		policy      v1beta1.SpokeDeletionPolicy
		wantOwnedBy bool
	}{
		"detach owns the secret":       {policy: v1beta1.SpokeDeletionPolicyDetach, wantOwnedBy: true},
		"unset policy owns the secret": {policy: "", wantOwnedBy: true},
		"orphan leaves it unowned":     {policy: v1beta1.SpokeDeletionPolicyOrphan, wantOwnedBy: false},
	}

	for name, tc := range cases {
		By(name, func() {
			sc := spoke("spoke", tc.policy)
			r := newTestReconciler(t, sc)

			if _, err := r.register(context.Background(), sc, tokenCredential()); err != nil {
				t.Fatalf("register returned an unexpected error: %v", err)
			}

			secret := readGatewaySecret(t, r.Client, sc.Name)
			if got := ownedBySpoke(secret, sc); got != tc.wantOwnedBy {
				t.Errorf("secret owned by the spoke = %v, want %v (refs %+v)", got, tc.wantOwnedBy, secret.OwnerReferences)
			}
		})
	}
})

// A detach SpokeCluster outside the gateway namespace cannot own the gateway Secret,
// because Kubernetes forbids cross-namespace owner references. Registration still has to
// succeed: the owner reference is only a garbage-collection backstop, and refusing to
// register would make the default deletion policy work in the gateway namespace alone.
// The owner annotation is still written, so the finalizer can identify what to clean up.
var _ = It("RegisterOutsideGatewayNamespaceSkipsOwnerRef", func() {
	t := GinkgoT()
	sc := spokeIn("spoke", "team-a", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)

	if _, err := r.register(context.Background(), sc, tokenCredential()); err != nil {
		t.Fatalf("register returned an unexpected error: %v", err)
	}

	secret := readGatewaySecret(t, r.Client, sc.Name)
	if ownedBySpoke(secret, sc) {
		t.Errorf("secret carries a cross-namespace owner reference, want none (refs %+v)", secret.OwnerReferences)
	}
	if got, want := secret.Annotations[secretOwnerAnnotation], sc.Namespace+"/"+sc.Name; got != want {
		t.Errorf("owner annotation = %q, want %q (the finalizer needs it to identify the secret)", got, want)
	}
})

// Requirements 6.2 and 6.3: registration reports what it did, so the caller can emit an
// event for a real change and stay silent for the identical rewrite a cached credential
// produces on every pass.
var _ = It("RegisterReportsWhatItDidToTheSecret", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)
	ctx := context.Background()

	outcome, err := r.register(ctx, sc, tokenCredential())
	Expect(err).NotTo(HaveOccurred())
	Expect(outcome).To(Equal(registerCreated))

	By("reporting an unchanged rewrite when the credential did not move", func() {
		outcome, err = r.register(ctx, sc, tokenCredential())
		Expect(err).NotTo(HaveOccurred())
		Expect(outcome).To(Equal(registerUnchanged),
			"a cached credential rewrites identical content and must not look like a change")
	})

	By("reporting an update when the token is reminted", func() {
		reminted := tokenCredential()
		reminted.Token = "tok-2"
		outcome, err = r.register(ctx, sc, reminted)
		Expect(err).NotTo(HaveOccurred())
		Expect(outcome).To(Equal(registerUpdated))
	})

	By("reporting an update when only the deletion-policy stamp moves", func() {
		// The stamp lives in an annotation, so a comparison limited to data and labels
		// would call this unchanged and lose the one event that records the flip.
		sc.Spec.DeletionPolicy = v1beta1.SpokeDeletionPolicyOrphan
		reminted := tokenCredential()
		reminted.Token = "tok-2"
		outcome, err = r.register(ctx, sc, reminted)
		Expect(err).NotTo(HaveOccurred())
		Expect(outcome).To(Equal(registerUpdated))
	})
})

var _ = It("RegisterIsIdempotent", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)
	ctx := context.Background()

	if _, err := r.register(ctx, sc, tokenCredential()); err != nil {
		t.Fatalf("first register returned an unexpected error: %v", err)
	}

	reminted := tokenCredential()
	reminted.Token = "tok-2"
	if _, err := r.register(ctx, sc, reminted); err != nil {
		t.Fatalf("second register returned an unexpected error: %v", err)
	}

	secret := readGatewaySecret(t, r.Client, sc.Name)
	if got := string(secret.Data["token"]); got != "tok-2" {
		t.Errorf("data[token] = %q, want the reminted %q", got, "tok-2")
	}
})

// A credential kind change has to replace the data keys wholesale, so the stale arm does
// not linger and confuse cluster-gateway, and the credential-type label has to flip.
var _ = It("RegisterReplacesCredentialKind", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)
	ctx := context.Background()

	if _, err := r.register(ctx, sc, tokenCredential()); err != nil {
		t.Fatalf("token register returned an unexpected error: %v", err)
	}
	if _, err := r.register(ctx, sc, certCredential()); err != nil {
		t.Fatalf("cert register returned an unexpected error: %v", err)
	}

	secret := readGatewaySecret(t, r.Client, sc.Name)
	if _, ok := secret.Data["token"]; ok {
		t.Error("data[token] survived the switch to a client cert, want it dropped")
	}
	if got := string(secret.Data["tls.crt"]); got != "cert-pem" {
		t.Errorf("data[tls.crt] = %q, want %q", got, "cert-pem")
	}
	if got := secret.Labels[credentialTypeLabel]; got != "X509Certificate" {
		t.Errorf("label %s = %q, want %q", credentialTypeLabel, got, "X509Certificate")
	}
})

// Flipping a registered spoke from detach to orphan has to clear the owner reference the
// earlier detach register set. Leaving it behind means garbage collection still reaps the
// Secret when the SpokeCluster is deleted, which is exactly what orphan promises not to
// do. The prototype only ever added references, so this is a deliberate deviation.
var _ = It("RegisterClearsOwnershipOnPolicyFlipToOrphan", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)
	ctx := context.Background()

	if _, err := r.register(ctx, sc, tokenCredential()); err != nil {
		t.Fatalf("detach register returned an unexpected error: %v", err)
	}
	if !ownedBySpoke(readGatewaySecret(t, r.Client, sc.Name), sc) {
		t.Fatal("detach register did not set the owner reference, cannot test the flip")
	}

	sc.Spec.DeletionPolicy = v1beta1.SpokeDeletionPolicyOrphan
	if _, err := r.register(ctx, sc, tokenCredential()); err != nil {
		t.Fatalf("orphan register returned an unexpected error: %v", err)
	}

	secret := readGatewaySecret(t, r.Client, sc.Name)
	if ownedBySpoke(secret, sc) {
		t.Errorf("owner reference survived the flip to orphan, refs %+v", secret.OwnerReferences)
	}
})

// Clearing ownership must be surgical: a controller reference owned by something else is
// not this controller's to remove. The fixture carries our own owner annotation, standing
// in for a secret this SpokeCluster registered previously that some other system has since
// also attached a controller reference to; without that annotation the adopt guard in
// verifyAdoptable would refuse to touch it at all (see TestRegisterRefusesToAdoptForeignSecret).
var _ = It("RegisterKeepsForeignOwnerReference", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyOrphan)
	foreign := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "someone-else",
		UID:        apitypes.UID("uid-someone-else"),
		Controller: ptr.To(true),
	}
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            sc.Name,
			Namespace:       multicluster.ClusterGatewaySecretNamespace,
			OwnerReferences: []metav1.OwnerReference{foreign},
			Annotations:     map[string]string{secretOwnerAnnotation: sc.Namespace + "/" + sc.Name},
		},
	}
	r := newTestReconciler(t, sc, existing)

	if _, err := r.register(context.Background(), sc, tokenCredential()); err != nil {
		t.Fatalf("register returned an unexpected error: %v", err)
	}

	secret := readGatewaySecret(t, r.Client, sc.Name)
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Name != foreign.Name {
		t.Errorf("owner references = %+v, want the foreign reference untouched", secret.OwnerReferences)
	}
})

// TestRegisterRefusesToAdoptForeignSecret is the fake-client counterpart to the live
// hijack finding: a gateway Secret with no owner annotation, standing in for one
// `vela cluster join` wrote by hand, must never be silently overwritten.
var _ = It("RegisterRefusesToAdoptForeignSecret", func() {
	t := GinkgoT()
	sc := spoke("victim", v1beta1.SpokeDeletionPolicyDetach)
	manuallyJoined := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sc.Name,
			Namespace: multicluster.ClusterGatewaySecretNamespace,
			Labels:    map[string]string{credentialTypeLabel: "X509Certificate"},
		},
		Data: map[string][]byte{"endpoint": []byte("https://other-cluster"), "token": []byte("original-tok")},
	}
	r := newTestReconciler(t, sc, manuallyJoined)

	_, err := r.register(context.Background(), sc, tokenCredential())
	if err == nil {
		t.Fatal("register adopted a foreign secret, want a refusal")
	}

	secret := readGatewaySecret(t, r.Client, sc.Name)
	if got := string(secret.Data["endpoint"]); got != "https://other-cluster" {
		t.Errorf("endpoint = %q, the foreign secret was overwritten despite the refusal", got)
	}
	if ownedBySpoke(secret, sc) {
		t.Error("the SpokeCluster took ownership of a foreign secret despite the refusal")
	}
})

// TestRegisterRefusesCrossNamespaceNameCollision is the same guard from a different
// angle: a gateway Secret already owned by a different, still-live SpokeCluster (a
// same-named SpokeCluster in another namespace, since the Secret's identity is name-only
// within the fixed gateway namespace) must not be taken over either.
var _ = It("RegisterRefusesCrossNamespaceNameCollision", func() {
	t := GinkgoT()
	other := spokeIn("shared-name", "team-a", v1beta1.SpokeDeletionPolicyDetach)
	mine := spokeIn("shared-name", "team-b", v1beta1.SpokeDeletionPolicyDetach)
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        other.Name,
			Namespace:   multicluster.ClusterGatewaySecretNamespace,
			Annotations: map[string]string{secretOwnerAnnotation: other.Namespace + "/" + other.Name},
		},
	}
	r := newTestReconciler(t, mine, existing)

	_, err := r.register(context.Background(), mine, tokenCredential())
	if err == nil {
		t.Fatal("register took over another SpokeCluster's secret, want a refusal")
	}

	secret := readGatewaySecret(t, r.Client, other.Name)
	if got := secret.Annotations[secretOwnerAnnotation]; got != other.Namespace+"/"+other.Name {
		t.Errorf("owner annotation = %q, want the original owner untouched", got)
	}
})

// A SpokeCluster's own re-register, after the guard, must still converge: the marker this
// controller writes on success is what makes the second call recognize the first's work.
var _ = It("RegisterOwnAnnotationAllowsReRegister", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)
	ctx := context.Background()

	if _, err := r.register(ctx, sc, tokenCredential()); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	if _, err := r.register(ctx, sc, tokenCredential()); err != nil {
		t.Fatalf("second register on the same SpokeCluster was refused: %v", err)
	}

	secret := readGatewaySecret(t, r.Client, sc.Name)
	if got := secret.Annotations[secretOwnerAnnotation]; got != sc.Namespace+"/"+sc.Name {
		t.Errorf("owner annotation = %q, want %q", got, sc.Namespace+"/"+sc.Name)
	}
})

// TestRegisterRejectsIncompatibleServerName covers the case cluster-gateway has no
// representation for: a kubeconfig tls-server-name that actually differs from the
// endpoint's own host. Registering it anyway would silently produce a Secret that fails
// TLS verification on every connection.
var _ = It("RegisterRejectsIncompatibleServerName", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)

	m := &credential.Materialized{
		Endpoint:   "https://10.0.0.5:6443",
		Token:      "tok",
		ServerName: "api.internal.example.com",
	}
	_, err := r.register(context.Background(), sc, m)
	if err == nil {
		t.Fatal("register accepted a ServerName that differs from the endpoint host, want a refusal")
	}
	if getErr := r.Get(context.Background(), gatewayKey(sc.Name), &corev1.Secret{}); !apierrors.IsNotFound(getErr) {
		t.Errorf("gateway secret was written despite the refusal: %v", getErr)
	}
})

// A ServerName that already matches the endpoint host loses nothing by being discarded,
// so registration must proceed normally.
var _ = It("RegisterAllowsServerNameMatchingEndpointHost", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)

	m := &credential.Materialized{
		Endpoint:   "https://api.internal.example.com:6443",
		CAData:     []byte("ca-pem"),
		Token:      "tok",
		ServerName: "api.internal.example.com",
	}
	if _, err := r.register(context.Background(), sc, m); err != nil {
		t.Fatalf("register refused a ServerName matching the endpoint host: %v", err)
	}
})

// DNS names are case-insensitive; a tls-server-name that differs only in case from the
// endpoint host must still be accepted.
var _ = It("RegisterAllowsServerNameMatchingEndpointHostCaseInsensitive", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)

	m := &credential.Materialized{
		Endpoint:   "https://api.internal.example.com:6443",
		CAData:     []byte("ca-pem"),
		Token:      "tok",
		ServerName: "API.Internal.Example.COM",
	}
	if _, err := r.register(context.Background(), sc, m); err != nil {
		t.Fatalf("register refused a case-equivalent ServerName: %v", err)
	}
})

var _ = It("RegisterRejectsBlockedProxyURL", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)

	m := tokenCredential()
	m.ProxyURL = "http://169.254.169.254/"
	_, err := r.register(context.Background(), sc, m)
	if err == nil {
		t.Fatal("register accepted a blocked proxy-url, want a refusal")
	}
	if !strings.Contains(err.Error(), "proxy-url") {
		t.Fatalf("error %q should mention proxy-url", err)
	}
	if getErr := r.Get(context.Background(), gatewayKey(sc.Name), &corev1.Secret{}); !apierrors.IsNotFound(getErr) {
		t.Errorf("gateway secret was written despite the refusal: %v", getErr)
	}
})

var _ = It("RegisterDropsProxyURLWhenCleared", func() {
	t := GinkgoT()
	sc := spoke("spoke", v1beta1.SpokeDeletionPolicyDetach)
	r := newTestReconciler(t, sc)
	ctx := context.Background()

	withProxy := tokenCredential()
	withProxy.ProxyURL = "http://10.0.0.1:8080"
	if _, err := r.register(ctx, sc, withProxy); err != nil {
		t.Fatalf("register with proxy-url: %v", err)
	}
	if _, err := r.register(ctx, sc, tokenCredential()); err != nil {
		t.Fatalf("register without proxy-url: %v", err)
	}
	secret := readGatewaySecret(t, r.Client, sc.Name)
	if _, ok := secret.Data["proxy-url"]; ok {
		t.Error("data[proxy-url] survived after the kubeconfig dropped it")
	}
})

// ownedBySpoke reports whether the secret carries a controller reference naming sc.
func ownedBySpoke(secret *corev1.Secret, sc *v1beta1.SpokeCluster) bool {
	for _, ref := range secret.OwnerReferences {
		if ref.Controller != nil && *ref.Controller && ref.Kind == v1beta1.SpokeClusterKind && ref.Name == sc.Name {
			return true
		}
	}
	return false
}
