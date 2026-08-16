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

// Package spokecluster reconciles SpokeCluster objects on the hub: it materializes a
// credential through the registered providers, upserts the cluster-gateway Secret that
// makes the spoke reachable, probes it, and tears the registration down on delete.
//
// The Secret this package writes is deliberately bit-compatible with what
// `vela cluster join` writes, so read-through, topology dispatch, and `vela cluster list`
// cannot tell a declaratively registered spoke from a manually joined one.
package spokecluster

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	recorder "github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/kubevela/pkg/util/k8s"
	clusterv1alpha1 "github.com/oam-dev/cluster-gateway/pkg/apis/cluster/v1alpha1"
	clustercommon "github.com/oam-dev/cluster-gateway/pkg/common"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/multicluster"
	"github.com/oam-dev/kubevela/pkg/spokecluster/credential"
)

// FinalizerName guards deletion so the controller can detach the spoke before the
// SpokeCluster is gone.
const FinalizerName = "spokecluster.core.oam.dev/finalizer"

// Gateway Secret data keys. These are cluster-gateway's contract, mirrored from
// (*KubeClusterConfig).createOrUpdateClusterSecret in pkg/multicluster, and must not
// drift from it: both writers have to stay interchangeable for every consumer.
const (
	secretKeyEndpoint = "endpoint"
	secretKeyCACert   = "ca.crt"
	secretKeyToken    = "token"
	secretKeyTLSCert  = "tls.crt"
	secretKeyTLSKey   = "tls.key"
	secretKeyProxyURL = "proxy-url"
)

// secretOwnerAnnotation is the controller-local alias of
// multicluster.SpokeClusterOwnerAnnotation. Join and rename refuse overwrite
// using the same key; keep this alias so register/janitor and `vela cluster join`
// cannot drift onto different annotation strings.
const secretOwnerAnnotation = multicluster.SpokeClusterOwnerAnnotation

// secretDeletionPolicyAnnotation records the SpokeCluster's deletionPolicy at the time the
// gateway Secret was last written. Cross-namespace spokes cannot use an OwnerReference
// backstop (Kubernetes forbids cross-namespace owners), so a force-deleted SpokeCluster
// would otherwise leak its Secret. The gateway-secret janitor uses this annotation to
// decide whether a Secret whose owner SpokeCluster is gone should be detached (default)
// or kept (orphan).
const secretDeletionPolicyAnnotation = "spokecluster.core.oam.dev/deletion-policy"

// verifyAdoptable refuses to touch a gateway Secret this SpokeCluster does not already
// own. A Secret with no owner annotation is foreign, most likely a manually joined
// cluster; adopting one is a Secret-migration concern, not this controller's. A Secret owned
// by a different namespace/name is a genuine collision: two SpokeClusters can share a
// name across namespaces, since the Secret's identity does not include the SpokeCluster's
// own namespace, and only one of them may hold it.
func verifyAdoptable(sc *v1beta1.SpokeCluster, secret *corev1.Secret) error {
	mine := sc.Namespace + "/" + sc.Name
	owner, ok := secret.Annotations[secretOwnerAnnotation]
	switch {
	case ok && owner == mine:
		return nil
	case !ok:
		return fmt.Errorf("gateway secret %s/%s already exists and is not managed by a SpokeCluster (likely a manually joined cluster); refusing to overwrite it", secret.Namespace, secret.Name)
	default:
		return fmt.Errorf("gateway secret %s/%s is owned by a different SpokeCluster (%s); refusing to overwrite it", secret.Namespace, secret.Name, owner)
	}
}

// markOwner stamps the gateway Secret with the SpokeCluster that wrote it and the
// deletionPolicy in force, so a later register call can tell this Secret apart from one it
// does not manage and the janitor can GC force-deleted cross-namespace spokes correctly.
func markOwner(sc *v1beta1.SpokeCluster, secret *corev1.Secret) {
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[secretOwnerAnnotation] = sc.Namespace + "/" + sc.Name
	policy := sc.Spec.DeletionPolicy
	if policy == "" {
		policy = v1beta1.SpokeDeletionPolicyDetach
	}
	secret.Annotations[secretDeletionPolicyAnnotation] = string(policy)
}

// ownsGatewaySecret reports whether the gateway Secret at sc's name, if any, is one this
// SpokeCluster registered itself (see verifyAdoptable). A missing Secret is not owned and
// not an error: there is nothing to clean up either way. This is the same check register
// uses before writing, reused here so deletion can never reach a Secret this SpokeCluster
// was refused permission to adopt in the first place.
func (r *Reconciler) ownsGatewaySecret(ctx context.Context, sc *v1beta1.SpokeCluster) (bool, error) {
	secret := &corev1.Secret{}
	key := gatewaySecretKey(sc)
	err := r.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to read gateway secret %s: %w", key, err)
	}
	return verifyAdoptable(sc, secret) == nil, nil
}

// verifyServerNameCompatible refuses a credential whose TLS server name override
// cluster-gateway has no way to honor. The gateway Secret carries no server-name key:
// cluster-gateway always derives the TLS ServerName it verifies against from the
// endpoint's own host (see cluster-gateway's transport.go). A kubeconfig `tls-server-name`
// that differs from the endpoint host would register successfully today and then fail TLS
// verification on every connection attempt, with nothing to surface the problem until the
// next probe. Refusing here surfaces it immediately instead of leaving a silently
// unreachable spoke. When the override already matches the endpoint host, nothing is lost by
// discarding it, so registration proceeds.
func verifyServerNameCompatible(m *credential.Materialized) error {
	if m.ServerName == "" {
		return nil
	}
	host, err := endpointHost(m.Endpoint)
	if err != nil {
		return fmt.Errorf("failed to parse endpoint %q: %w", m.Endpoint, err)
	}
	if m.ServerName != host {
		return fmt.Errorf("credential requires TLS server name %q, but cluster-gateway always verifies against the endpoint host %q for this substrate; there is no way to honor a differing server name", m.ServerName, host)
	}
	return nil
}

// endpointHost extracts the bare host cluster-gateway's transport derives its TLS
// ServerName from: the endpoint URL's host with any port stripped.
func endpointHost(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	host, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		var addrErr *net.AddrError
		if errors.As(err, &addrErr) && addrErr.Err == "missing port in address" {
			return u.Host, nil
		}
		return "", err
	}
	return host, nil
}

// Reconciler reconciles a SpokeCluster object. The reconcile loop itself, along with the
// conditions, probe, requeue policy and status write, lives in
// spokecluster_controller.go; this file holds the registration half.
//
// Config is the hub rest.Config the probe and discovery reach spokes through. It is copied
// per call rather than shared, because RequestRawK8sAPIForCluster mutates the config it is
// given and then resets the mutated fields to nil rather than to their previous values,
// which races across concurrent reconciles.
//
// probeFn and discoverFn are test seams. Nil means the real method, so no unit test needs a
// live spoke or a real rest.Config.
type Reconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Config    *rest.Config
	Providers credential.Registry

	// SpokeReader reads objects on the spoke rather than the hub. Its transport turns a
	// cluster-name context into a cluster-gateway request, which the embedded Client
	// cannot do: that one is the manager's cache-backed client, so it answers the same
	// List from the hub's own informers and silently reports the hub's inventory as the
	// spoke's. Discovery must therefore go through this client, never through Client.
	SpokeReader client.Client

	// SecretReader is an uncached hub reader for source kubeconfig Secrets. The
	// manager caches Secrets only in the gateway namespace (RBAC-01); tenant-ns
	// Gets must bypass that cache so we never start a cluster-wide Secret informer.
	SecretReader client.Reader

	record recorder.Recorder

	concurrentReconciles int

	// credentials holds the last materialized credential per spoke, so a pass whose
	// credential is still well short of its refresh deadline can skip Materialize. For
	// the aws arm that call is an sts:AssumeRole plus an eks:DescribeCluster every pass,
	// re-deriving an endpoint and CA that are fixed for the cluster's lifetime.
	//
	// A nil cache means caching is off and is fully supported: every method is nil-safe,
	// so a Reconciler built directly behaves exactly as it did before this field existed.
	// Setup installs a real one.
	//
	// Spec change, delete, source-Secret watch, a 401 from the spoke, or a controller
	// restart evicts an entry early. Editing a label or an annotation does not, because
	// neither bumps metadata.generation, so there is no "annotate the object to force a
	// refresh". Source kubeconfig Secret create/update/delete goes through
	// mapKubeconfigSecret, which Invalidates before enqueue so a rotate is not held
	// behind NextRefresh.
	credentials *credentialCache

	probeFn    func(ctx context.Context, sc *v1beta1.SpokeCluster) (time.Duration, error)
	discoverFn func(ctx context.Context, sc *v1beta1.SpokeCluster, m *credential.Materialized, latency time.Duration) (*v1beta1.SpokeClusterInfo, error)
}

// gatewaySecretKey is where the gateway Secret for a spoke lives: named after the
// SpokeCluster, in the resolved gateway namespace (vela-system by default). The namespace
// is a package-level var in pkg/multicluster resolved at startup, so it is read per call
// rather than captured.
func gatewaySecretKey(sc *v1beta1.SpokeCluster) apitypes.NamespacedName {
	return apitypes.NamespacedName{Name: sc.Name, Namespace: multicluster.ClusterGatewaySecretNamespace}
}

// secretReader returns the uncached hub reader for source Secrets, falling back to the
// embedded Client so unit tests that construct a Reconciler with only a fake client keep
// working.
func (r *Reconciler) secretReader() client.Reader {
	if r.SecretReader != nil {
		return r.SecretReader
	}
	return r.Client
}

// register upserts the cluster-gateway Secret from the materialized credential, in the
// shape `vela cluster join` writes: name = cluster name, namespace = the gateway
// namespace, type Opaque, data.endpoint, data["ca.crt"] when the CA is known, then either
// data.token or the data["tls.crt"]/data["tls.key"] pair, labelled with the credential
// type. That label is what makes cluster-gateway surface the Secret as a virtual cluster.
//
// Registration is idempotent by get-then-create-or-update: Data and the credential-type
// label are replaced wholesale, so a reminted token converges and a kind change (token to
// cert or back) drops the stale arm instead of leaving both present. Create and Update are
// not transactional, so a concurrent writer can turn the Create into an AlreadyExists
// error; that returns for controller-runtime backoff and the next pass converges, which is
// the same posture as the join path.
//
// Two properties of the gateway Secret shape are worth knowing at the call site:
//
//   - Materialized.ServerName has nowhere to go: the Secret carries no server-name key, and
//     cluster-gateway always derives the TLS ServerName from the endpoint host itself. A
//     kubeconfig tls-server-name that actually differs from the endpoint host is refused
//     (see verifyServerNameCompatible) rather than silently registered and left to fail TLS
//     verification on every connection.
//   - An absent ca.crt historically meant an insecure endpoint to cluster-gateway
//     (not "verify against the system roots"). Kubeconfig materialization
//     rejects insecure-skip-tls-verify and missing certificate-authority-data,
//     and register refuses an empty CA bundle so a future provider cannot
//     reopen skip-verify by omission.
//   - Materialized.ProxyURL is written to data["proxy-url"] so a proxied
//     kubeconfig matches what `vela cluster join` already records. Empty means
//     the key is omitted (direct dial).
//
// register refuses to adopt a pre-existing Secret it does not recognize (see
// verifyAdoptable): design.md reasoned that the admission webhook already
// rejects a name collision with an existing gateway Secret, but that webhook is stateless
// by design and does not read Secrets, so no such check exists. Without this guard a
// SpokeCluster could silently take over a manually joined cluster's Secret, redirecting
// its traffic to a different cluster and, under detach, later deleting its credential
// when the SpokeCluster itself is deleted. Confirmed live against a real `vela cluster
// join` fixture before this guard existed.
func (r *Reconciler) register(ctx context.Context, sc *v1beta1.SpokeCluster, m *credential.Materialized) error {
	// Reserved name: admission and reconcile SpecInvalid already reject this, but
	// register is the last write before a gateway Secret named "local" would
	// collide with in-process hub routing.
	if sc.Name == multicluster.ClusterLocalName {
		return fmt.Errorf("refusing to register reserved cluster name %q", multicluster.ClusterLocalName)
	}
	secret := &corev1.Secret{}
	key := gatewaySecretKey(sc)
	err := r.Get(ctx, key, secret)
	notFound := apierrors.IsNotFound(err)
	if err != nil && !notFound {
		return fmt.Errorf("failed to read gateway secret %s: %w", key, err)
	}
	if !notFound {
		if err := verifyAdoptable(sc, secret); err != nil {
			return err
		}
	}
	if err := verifyServerNameCompatible(m); err != nil {
		return err
	}
	// Defense in depth: every provider must pass endpoint policy, but register is the
	// last gate before the gateway Secret is written (covers mocks and future providers).
	if err := credential.ValidateSpokeEndpoint(m.Endpoint); err != nil {
		return err
	}
	if err := credential.ValidateSpokeProxyURL(m.ProxyURL); err != nil {
		return err
	}
	if len(m.CAData) == 0 {
		return fmt.Errorf("refusing to register spoke %q without a CA bundle; empty ca.crt would make cluster-gateway skip TLS verification", sc.Name)
	}

	secret.Name = key.Name
	secret.Namespace = key.Namespace
	secret.Type = corev1.SecretTypeOpaque

	data := map[string][]byte{
		secretKeyEndpoint: []byte(m.Endpoint),
		secretKeyCACert:   m.CAData,
	}
	var credType clusterv1alpha1.CredentialType
	switch {
	case m.HasClientCert():
		credType = clusterv1alpha1.CredentialTypeX509Certificate
		data[secretKeyTLSCert] = m.ClientCertData
		data[secretKeyTLSKey] = m.ClientKeyData
	default:
		credType = clusterv1alpha1.CredentialTypeServiceAccountToken
		data[secretKeyToken] = []byte(m.Token)
	}
	if m.ProxyURL != "" {
		data[secretKeyProxyURL] = []byte(m.ProxyURL)
	}
	secret.Data = data
	_ = k8s.AddLabel(secret, clustercommon.LabelKeyClusterCredentialType, string(credType))
	markOwner(sc, secret)

	if err := r.reconcileOwnership(sc, secret); err != nil {
		return err
	}

	if notFound {
		return r.Create(ctx, secret)
	}
	return r.Update(ctx, secret)
}

// reconcileOwnership brings the gateway Secret's owner reference in line with
// spec.deletionPolicy. Under detach (the default, and an empty policy on objects that
// predate schema defaulting) the SpokeCluster owns the Secret, so garbage collection
// removes it even when the finalizer is bypassed by force. Under orphan no reference is
// left, so GC cannot reap the Secret the policy promises to keep. The finalizer is the
// primary cleanup mechanism in both cases; the owner reference is only a backstop.
//
// Clearing on the orphan path is a deliberate deviation from the prototype, which only
// ever added a reference. Without it, flipping a registered spoke from detach to orphan
// leaves the old controller reference in place and GC still deletes the Secret once the
// SpokeCluster is gone, the opposite of what the policy promises.
func (r *Reconciler) reconcileOwnership(sc *v1beta1.SpokeCluster, secret *corev1.Secret) error {
	if sc.Spec.DeletionPolicy == v1beta1.SpokeDeletionPolicyOrphan {
		clearControllerRef(sc, secret)
		return nil
	}
	// SpokeCluster is namespaced and the gateway Secret always lives in the gateway
	// namespace, so a spoke declared anywhere else cannot own it: Kubernetes forbids
	// cross-namespace owner references. Skip the backstop rather than fail registration.
	// Failing would make the default deletion policy work only for spokes declared in the
	// gateway namespace, while orphan worked everywhere, which is not a distinction the API
	// expresses. What is lost is narrow: reconcileDelete still removes the Secret on every
	// ordinary deletion and is namespace-independent, so only a force-deleted SpokeCluster
	// (finalizer patched off, or the controller permanently down) leaves the Secret behind.
	//
	// The skip is logged at V(4) rather than Info because it is a standing property of where
	// the spoke is declared, not an event: register runs on every reconcile pass, so an Info
	// line here would repeat once per probe interval for the lifetime of the spoke.
	if sc.Namespace != secret.Namespace {
		klog.V(4).InfoS("Skipping gateway secret owner reference: owner references cannot cross namespaces",
			"spokecluster", klog.KRef(sc.Namespace, sc.Name),
			"secret", klog.KRef(secret.Namespace, secret.Name),
			"consequence", "the finalizer is the only cleanup path for this spoke")
		return nil
	}
	if err := controllerutil.SetControllerReference(sc, secret, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on gateway secret: %w", err)
	}
	return nil
}

// clearControllerRef drops the controller owner reference this SpokeCluster owns, leaving
// references owned by anything else untouched. It matches on UID when both sides have one
// and falls back to kind plus name otherwise.
//
// controllerutil.RemoveControllerReference is deliberately not used: it returns an error
// when the object carries no controller reference at all, which is the ordinary orphan
// case, so telling that apart from a real failure would mean matching on its error text.
func clearControllerRef(sc *v1beta1.SpokeCluster, secret *corev1.Secret) {
	refs := secret.GetOwnerReferences()
	kept := make([]metav1.OwnerReference, 0, len(refs))
	for _, ref := range refs {
		if isControllerRefFor(ref, sc) {
			continue
		}
		kept = append(kept, ref)
	}
	if len(kept) != len(refs) {
		secret.SetOwnerReferences(kept)
	}
}

// isControllerRefFor reports whether ref is a controller reference naming this
// SpokeCluster.
func isControllerRefFor(ref metav1.OwnerReference, sc *v1beta1.SpokeCluster) bool {
	if ref.Controller == nil || !*ref.Controller {
		return false
	}
	if sc.UID != "" && ref.UID != "" {
		return ref.UID == sc.UID
	}
	return ref.Kind == v1beta1.SpokeClusterKind && ref.Name == sc.Name
}

// deleteGatewaySecret removes the gateway Secret if this SpokeCluster owns it. Not-found is
// success on both the read and the delete, so every deletion path is idempotent. A Secret
// this SpokeCluster does not own (never registered, or refused earlier by the adopt guard
// in register/verifyAdoptable) is left alone: deletion must never reach a manually joined
// cluster's Secret, or a different SpokeCluster's, just because a same-named SpokeCluster
// is being cleaned up.
func (r *Reconciler) deleteGatewaySecret(ctx context.Context, sc *v1beta1.SpokeCluster) error {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, gatewaySecretKey(sc), secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if verifyAdoptable(sc, secret) != nil {
		return nil
	}
	uid := secret.UID
	return client.IgnoreNotFound(r.Delete(ctx, secret, client.Preconditions{UID: &uid}))
}
