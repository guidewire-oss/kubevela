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

	"github.com/crossplane/crossplane-runtime/pkg/event"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/multicluster"
)

// reconcileDelete runs finalizer cleanup and then releases the finalizer. Under detach
// (the default, and an empty policy) it detaches the spoke from cluster-gateway, which
// scrubs ResourceTracker references to it and deletes the gateway Secret. Under orphan it
// leaves the registration in place.
//
// Adding the finalizer on the create path belongs to the reconcile loop; a
// SpokeCluster that never carried it has nothing to clean up.
//
// Cleanup only ever touches a gateway Secret this SpokeCluster owns (see ownsGatewaySecret).
// A SpokeCluster whose name collided with a manually joined cluster, or with a different
// SpokeCluster across namespaces, was refused registration by the adopt guard in the first
// place and so never wrote anything of its own; deleting it must not reach for someone
// else's Secret just because the names match.
func (r *Reconciler) reconcileDelete(ctx context.Context, sc *v1beta1.SpokeCluster) (ctrl.Result, error) {
	// Unconditional, and before anything else. A SpokeCluster whose finalizer was already
	// removed returns below without reaching any cleanup, and a detach that has to be
	// retried must not leave a live entry between attempts. The purge is an O(1) delete
	// with no failure mode, so gating it on either would only create ways to skip it.
	r.credentials.Invalidate(client.ObjectKeyFromObject(sc))

	if !controllerutil.ContainsFinalizer(sc, FinalizerName) {
		return ctrl.Result{}, nil
	}

	owned, err := r.ownsGatewaySecret(ctx, sc)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Detached events and spokeDetachTotal only fire when we actually tried to detach:
	// an owned Secret under detach (or the unset default). Orphan and never-owned
	// deletions still release the finalizer, but they are not a detach.
	attemptedDetach := owned && sc.Spec.DeletionPolicy != v1beta1.SpokeDeletionPolicyOrphan
	if attemptedDetach {
		if err := multicluster.DetachCluster(ctx, r.Client, sc.Name); err != nil {
			if !isExpectedDetachFailure(err) {
				// A ResourceTracker scrub failure, or any other unexpected error, must be
				// retried rather than treated as done: falling back to a direct secret
				// delete here would release the finalizer while stale ResourceTracker
				// references to this cluster are left behind.
				return ctrl.Result{}, fmt.Errorf("failed to detach spoke cluster %s: %w", sc.Name, err)
			}
			// The spoke never finished registering (no gateway Secret, so GetVirtualCluster
			// reports not found) or hit the reserved `local` name (structurally impossible
			// for a SpokeCluster given admission validation, checked defensively). Falling
			// back to a direct delete keeps deletion from wedging on a half-registered
			// spoke.
			klog.InfoS("DetachCluster returned an expected error during SpokeCluster deletion, attempting direct secret cleanup",
				"spokecluster", klog.KObj(sc), "err", err)
			if delErr := r.deleteGatewaySecret(ctx, sc); delErr != nil {
				return ctrl.Result{}, delErr
			}
		}
	}

	// Under detach the Secret may also carry an owner reference, so two deleters exist for
	// it. Ordering makes that safe: cleanup here runs while the owner still exists, and the
	// later garbage-collection pass finds nothing to do.
	controllerutil.RemoveFinalizer(sc, FinalizerName)
	if err := r.Update(ctx, sc); err != nil {
		if attemptedDetach {
			spokeDetachTotal.WithLabelValues(sc.Namespace, sc.Name, metricResultError).Inc()
		}
		return ctrl.Result{}, err
	}
	if attemptedDetach {
		r.emit(sc, event.Normal(reasonDetached, "spoke cluster detached"))
		spokeDetachTotal.WithLabelValues(sc.Namespace, sc.Name, metricResultSuccess).Inc()
	}
	// After the finalizer is released, so a retried deletion cannot drop a live spoke's
	// series, and for every deletion path: the series belong to the object, not to whether
	// a detach was attempted.
	forgetSpokeMetrics(sc)
	return ctrl.Result{}, nil
}

// isExpectedDetachFailure reports whether err from DetachCluster is one of the two benign
// cases worth swallowing and falling back to a direct secret delete: the spoke never
// finished registering, or the reserved `local` cluster name. Any other failure, notably a
// ResourceTracker scrub error (DetachCluster runs that scrub before it ever looks at
// whether the spoke is registered), must be retried instead of silently treated as done.
func isExpectedDetachFailure(err error) bool {
	return multicluster.IsNotFoundOrClusterNotExists(err) || errors.Is(err, multicluster.ErrReservedLocalClusterName)
}
