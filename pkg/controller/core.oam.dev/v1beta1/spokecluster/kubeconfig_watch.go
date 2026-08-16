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

	clustercommon "github.com/oam-dev/cluster-gateway/pkg/common"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

func kubeconfigSecretIndexKeys(sc *v1beta1.SpokeCluster) []string {
	if sc.Spec.Credential.Type != v1beta1.CredentialTypeKubeconfig || sc.Spec.Credential.Kubeconfig == nil {
		return nil
	}
	ref := sc.Spec.Credential.Kubeconfig.SecretRef
	if ref.Name == "" {
		return nil
	}
	ns := ref.Namespace
	if ns == "" {
		ns = sc.Namespace
	}
	return []string{ns + "/" + ref.Name}
}

// sourceKubeconfigSecretPredicate drops gateway Secrets this controller writes.
// Those live in the same informer (the gateway namespace) and would otherwise
// requeue the SpokeCluster on every register pass.
var sourceKubeconfigSecretPredicate = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool { return !isGatewayClusterSecret(e.Object) },
	UpdateFunc: func(e event.UpdateEvent) bool { return !isGatewayClusterSecret(e.ObjectNew) },
	DeleteFunc: func(e event.DeleteEvent) bool { return !isGatewayClusterSecret(e.Object) },
}

func isGatewayClusterSecret(obj client.Object) bool {
	if obj == nil {
		return false
	}
	if obj.GetAnnotations()[secretOwnerAnnotation] != "" {
		return true
	}
	if obj.GetLabels()[clustercommon.LabelKeyClusterCredentialType] != "" {
		return true
	}
	return false
}

func (r *Reconciler) mapKubeconfigSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj == nil {
		return nil
	}
	key := obj.GetNamespace() + "/" + obj.GetName()
	list := &v1beta1.SpokeClusterList{}
	if err := r.List(ctx, list); err != nil {
		klog.ErrorS(err, "failed to list SpokeClusters for kubeconfig Secret", "secret", klog.KObj(obj))
		return nil
	}
	// Filter in process rather than MatchingFields. A custom field index is easy
	// to get wrong against the manager cache, and the SpokeCluster list is
	// already in memory. Gateway Secret updates never reach here (predicate).
	//
	// Drop any cached credential for a matching spoke before enqueue. JWT and
	// client-cert kubeconfigs carry a NextRefresh and would otherwise keep
	// serving the pre-rotate material until that deadline (up to the cache TTL),
	// even though this watch already requeues the SpokeCluster.
	reqs := make([]reconcile.Request, 0, 1)
	for i := range list.Items {
		for _, indexed := range kubeconfigSecretIndexKeys(&list.Items[i]) {
			if indexed == key {
				sc := &list.Items[i]
				r.credentials.Invalidate(client.ObjectKeyFromObject(sc))
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(sc)})
				break
			}
		}
	}
	return reqs
}
