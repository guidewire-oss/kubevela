/*
 Copyright 2026. The KubeVela Authors.

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

package operation

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v2alpha1"
)

// operationLockDuration is how long a lock is honored without a renew
// before another Operation may take it over.
//
// TODO(KEP 2.15): only renewed once per Reconcile, not while ExecuteRunners
// is running a step -- a step blocking longer than this can still lose its
// lock mid-run. A goroutine renewing on a tick during ExecuteRunners would
// close that gap; deferred to keep this POC's scope small.
const operationLockDuration = 60 * time.Second

// operationLockName derives a stable Lease name for the (namespace, target,
// cluster) an Operation runs against, so any two Operations racing for the
// same target serialize on the same object regardless of their own names.
func operationLockName(target v2alpha1.OperationTarget, cluster string) string {
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%s/%s/%s", target.App, target.Component, cluster)
	return fmt.Sprintf("operation-lock-%x", h.Sum32())
}

// acquireLock tries to become (or remain) the holder of the Lease for op's
// target+cluster, so at most one Operation runs its workflow against a
// given target at a time. It succeeds if the Lease doesn't exist yet, is
// already held by op, or hasn't been renewed within operationLockDuration
// (the previous holder is presumed gone).
func (r *Reconciler) acquireLock(ctx context.Context, op *v2alpha1.Operation) (bool, error) {
	holder := string(op.UID)
	name := operationLockName(op.Spec.Target, localCluster)
	now := metav1.NewMicroTime(time.Now())

	lease := &coordinationv1.Lease{}
	err := r.Get(ctx, client.ObjectKey{Namespace: op.Namespace, Name: name}, lease)
	if kerrors.IsNotFound(err) {
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: op.Namespace,
				Labels: map[string]string{
					"operation.oam.dev/target-app":       op.Spec.Target.App,
					"operation.oam.dev/target-component": op.Spec.Target.Component,
					"operation.oam.dev/cluster":          localCluster,
				},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To(holder),
				LeaseDurationSeconds: ptr.To(int32(operationLockDuration.Seconds())),
				RenewTime:            &now,
			},
		}
		if err := r.Create(ctx, lease); err != nil {
			if kerrors.IsAlreadyExists(err) {
				// Lost the race to create it; the next reconcile will see
				// it as an existing lease and retry from there.
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}

	held := lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == holder
	expired := lease.Spec.RenewTime == nil || time.Since(lease.Spec.RenewTime.Time) > operationLockDuration
	if !held && !expired {
		return false, nil
	}

	lease.Spec.HolderIdentity = ptr.To(holder)
	lease.Spec.LeaseDurationSeconds = ptr.To(int32(operationLockDuration.Seconds()))
	lease.Spec.RenewTime = &now
	if err := r.Update(ctx, lease); err != nil {
		if kerrors.IsConflict(err) {
			// Someone else renewed or stole it first; retry next reconcile.
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// releaseLock deletes op's target+cluster Lease, but only if op still holds
// it -- releasing a lease already re-acquired by someone else would defeat
// the guarantee it exists to provide. Safe to call even if op never
// acquired the lock: it's then either absent or held by someone else, and
// this is a no-op either way.
func (r *Reconciler) releaseLock(ctx context.Context, op *v2alpha1.Operation) error {
	name := operationLockName(op.Spec.Target, localCluster)
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: op.Namespace, Name: name}, lease); err != nil {
		return client.IgnoreNotFound(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(op.UID) {
		return nil
	}
	// Precondition guards against a renew/steal between the Get and here:
	// the Delete fails instead of removing someone else's Lease.
	err := r.Delete(ctx, lease, client.Preconditions{UID: &lease.UID, ResourceVersion: &lease.ResourceVersion})
	if kerrors.IsConflict(err) {
		return nil
	}
	return client.IgnoreNotFound(err)
}
