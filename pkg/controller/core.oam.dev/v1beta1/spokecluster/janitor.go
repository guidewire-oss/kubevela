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

	// Re-read, then delete only that UID. A SpokeCluster recreated under the
	// same name between this check and the delete must not lose its new Secret:
	// the UID precondition fails if another Secret already replaced this one.
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
	rv := fresh.ResourceVersion
	clusterName := fresh.Name

	// LIFE-01: scrub ResourceTrackers while the orphan Secret still exists so a
	// scrub failure is retryable (the next janitor pass still sees the Secret).
	// Skip scrub and delete when a SpokeCluster already reclaims the gateway name:
	// the recreated spoke may have adopted this Secret (same UID) via verifyAdoptable.
	list := &v1beta1.SpokeClusterList{}
	if listErr := r.List(ctx, list); listErr != nil {
		return fmt.Errorf("gateway secret janitor failed listing SpokeClusters before ResourceTracker scrub: %w", listErr)
	}
	for i := range list.Items {
		if list.Items[i].Name == clusterName {
			klog.InfoS("gateway secret janitor skipping reclaim; SpokeCluster name is in use again",
				"cluster", clusterName, "spokecluster", klog.KObj(&list.Items[i]))
			return nil
		}
	}
	if scrubErr := multicluster.RemoveClusterFromResourceTrackers(ctx, r.Client, clusterName); scrubErr != nil {
		return fmt.Errorf("gateway secret janitor failed scrubbing ResourceTrackers for %s: %w", clusterName, scrubErr)
	}
	return client.IgnoreNotFound(r.Delete(ctx, fresh, client.Preconditions{UID: &uid, ResourceVersion: &rv}))
}

func parseSecretOwner(owner string) (namespace, name string, ok bool) {
	ns, name, found := strings.Cut(owner, "/")
	if !found || ns == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return ns, name, true
}
