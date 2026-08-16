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
	"time"

	clustercommon "github.com/oam-dev/cluster-gateway/pkg/common"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/multicluster"
)

// gatewaySecretJanitorInterval is how often the controller sweeps gateway Secrets whose
// owning SpokeCluster was force-deleted (finalizer bypassed). Cross-namespace spokes cannot
// rely on OwnerReference GC, so this is the backstop that UID-deletes the leaked Secret.
const gatewaySecretJanitorInterval = 30 * time.Second

// StartGatewaySecretJanitor runs a periodic sweep until ctx is cancelled. It is registered
// as a manager.Runnable from Setup so it shares the controller process lifetime.
func (r *Reconciler) StartGatewaySecretJanitor(ctx context.Context) error {
	// One pass immediately so a leaked Secret is not stuck waiting a full interval after
	// controller restart.
	r.sweepOrphanedGatewaySecrets(ctx)
	ticker := time.NewTicker(gatewaySecretJanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.sweepOrphanedGatewaySecrets(ctx)
		}
	}
}

// sweepOrphanedGatewaySecrets deletes gateway Secrets that still carry a SpokeCluster
// owner annotation after that SpokeCluster has been deleted, unless the recorded
// deletionPolicy is orphan. Manually joined Secrets (no owner annotation) are left alone.
func (r *Reconciler) sweepOrphanedGatewaySecrets(ctx context.Context) {
	var secrets corev1.SecretList
	// Only credential-labeled Secrets are candidates. Admission and TLS Secrets
	// in the same namespace are skipped at the List so the janitor never pulls them.
	if err := r.List(ctx, &secrets,
		client.InNamespace(multicluster.ClusterGatewaySecretNamespace),
		client.HasLabels{clustercommon.LabelKeyClusterCredentialType},
	); err != nil {
		klog.ErrorS(err, "gateway secret janitor failed to list secrets",
			"namespace", multicluster.ClusterGatewaySecretNamespace)
		return
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if err := r.reapGatewaySecretIfOwnerGone(ctx, secret); err != nil {
			klog.ErrorS(err, "gateway secret janitor failed to reap secret",
				"secret", klog.KObj(secret))
		}
	}
}

func (r *Reconciler) reapGatewaySecretIfOwnerGone(ctx context.Context, secret *corev1.Secret) error {
	if secret.Annotations == nil {
		return nil
	}
	owner := secret.Annotations[secretOwnerAnnotation]
	if owner == "" {
		return nil
	}
	if _, ok := secret.Labels[clustercommon.LabelKeyClusterCredentialType]; !ok {
		return nil
	}
	policy := secret.Annotations[secretDeletionPolicyAnnotation]
	if policy == string(v1beta1.SpokeDeletionPolicyOrphan) {
		return nil
	}
	ns, name, ok := parseSecretOwner(owner)
	if !ok {
		klog.InfoS("gateway secret janitor skipping secret with malformed owner annotation",
			"secret", klog.KObj(secret), "owner", owner)
		return nil
	}
	sc := &v1beta1.SpokeCluster{}
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, sc)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	// Re-read, then delete only that UID. DetachCluster looks up the Secret by
	// cluster name and scrubs ResourceTrackers for that name, so a SpokeCluster
	// recreated between this check and the delete would lose its new registration.
	// The janitor is the force-delete backstop; it must not impersonate a detach
	// of whatever currently holds the name. Stale ResourceTracker refs are left
	// for the next owned detach (or stay, if the name is reused on purpose).
	fresh := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(secret), fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	if fresh.UID != secret.UID {
		return nil
	}
	if fresh.Annotations == nil || fresh.Annotations[secretOwnerAnnotation] != owner {
		return nil
	}
	if fresh.ResourceVersion != secret.ResourceVersion {
		return nil
	}

	klog.InfoS("gateway secret janitor reclaiming Secret whose SpokeCluster is gone",
		"secret", klog.KObj(fresh), "owner", owner, "deletionPolicy", policy)
	uid := fresh.UID
	return client.IgnoreNotFound(r.Delete(ctx, fresh, client.Preconditions{UID: &uid}))
}

func parseSecretOwner(owner string) (namespace, name string, ok bool) {
	ns, name, found := strings.Cut(owner, "/")
	if !found || ns == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return ns, name, true
}
