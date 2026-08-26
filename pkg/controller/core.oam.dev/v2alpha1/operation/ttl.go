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
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v2alpha1"
)

// sweepTTL deletes op once its TTL (spec.ttlSecondsAfterFinished if set,
// else r.DefaultOperationTTL) has elapsed since CompletionTime. Called both
// right after an Operation reaches a terminal phase and on every subsequent
// Reconcile of an already-terminal Operation (the RequeueAfter below is
// what causes that second call).
//
// A restart resets CompletionTime to nil before this ever runs again (see
// pkg/workflow/operation's restartFrom), so a restarted Operation is
// naturally exempt until it goes terminal again.
func (r *Reconciler) sweepTTL(ctx context.Context, op *v2alpha1.Operation) (ctrl.Result, error) {
	if op.Status.CompletionTime == nil {
		return ctrl.Result{}, nil
	}
	ttl := r.DefaultOperationTTL
	if op.Spec.TTLSecondsAfterFinished != nil {
		ttl = time.Duration(*op.Spec.TTLSecondsAfterFinished) * time.Second
	}
	if ttl <= 0 {
		return ctrl.Result{}, nil
	}
	remaining := ttl - time.Since(op.Status.CompletionTime.Time)
	if remaining <= 0 {
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, op))
	}
	return ctrl.Result{RequeueAfter: remaining}, nil
}
