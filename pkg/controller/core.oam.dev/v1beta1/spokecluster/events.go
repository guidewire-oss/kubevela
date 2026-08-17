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
	"fmt"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

// Event reasons beyond the condition-reason constants. Detached is delete-path only;
// the two gateway-Secret reasons are registration-path only.
const (
	reasonDetached             = "Detached"
	reasonGatewaySecretCreated = "GatewaySecretCreated"
	reasonGatewaySecretUpdated = "GatewaySecretUpdated"
)

// emit records an event when a recorder is wired. Unit tests that build a Reconciler
// without SetupWithManager leave record nil and stay silent.
func (r *Reconciler) emit(obj runtime.Object, e event.Event) {
	if r == nil || r.record == nil {
		return
	}
	r.record.Event(obj, e)
}

// emitStatusEvents fires Kubernetes events only on meaningful status transitions so a
// steadily connected spoke on a 30s probe interval does not flood the Events API.
//
// Transition table:
//   - Connection -> Connected: Normal ProbeSucceeded
//   - Connection -> Disconnected: Warning ProbeFailed
//   - CredentialValid False (new): Warning MaterializeFailed / NoProvider / SpecInvalid
//   - Registered False (new): Warning RegisterFailed
//   - InfoSynced False (new): Warning DiscoveryFailed
//
// Connection -> Unknown deliberately has no event of its own. It is always the
// consequence of a credential or registration failure that already emitted, so a second
// event would double-report one cause.
func (r *Reconciler) emitStatusEvents(sc *v1beta1.SpokeCluster, prev, next *v1beta1.SpokeClusterStatus) {
	if next == nil {
		return
	}
	var prevConn v1beta1.ConnectionState
	if prev != nil {
		prevConn = prev.Connection
	}
	if next.Connection != prevConn {
		switch next.Connection {
		case v1beta1.ConnectionStateConnected:
			r.emit(sc, event.Normal(reasonProbeSucceeded, conditionMessage(next, v1beta1.SpokeClusterConditionConnected)))
			countConnectionTransition(sc, v1beta1.ConnectionStateConnected)
		case v1beta1.ConnectionStateDisconnected:
			msg := conditionMessage(next, v1beta1.SpokeClusterConditionConnected)
			r.emit(sc, event.Warning(reasonProbeFailed, fmt.Errorf("%s", msg)))
			countConnectionTransition(sc, v1beta1.ConnectionStateDisconnected)
		}
	}

	emitWarningOnConditionFalse(r, sc, prev, next, v1beta1.SpokeClusterConditionCredentialValid)
	emitWarningOnConditionFalse(r, sc, prev, next, v1beta1.SpokeClusterConditionRegistered)
	emitWarningOnConditionFalse(r, sc, prev, next, v1beta1.SpokeClusterConditionInfoSynced)
}

func countConnectionTransition(sc *v1beta1.SpokeCluster, to v1beta1.ConnectionState) {
	spokeConnectionTransitions.WithLabelValues(sc.Namespace, sc.Name, string(to)).Inc()
}

// emitGatewaySecretEvent reports what registration did to the materialized gateway
// Secret. An unchanged rewrite stays silent: with the credential cache serving a stable
// token, identical content is the steady state for a healthy spoke, and an event per pass
// would bury the transitions that matter.
func (r *Reconciler) emitGatewaySecretEvent(sc *v1beta1.SpokeCluster, outcome registerOutcome) {
	switch outcome {
	case registerCreated:
		r.emit(sc, event.Normal(reasonGatewaySecretCreated, "gateway credential secret created"))
	case registerUpdated:
		r.emit(sc, event.Normal(reasonGatewaySecretUpdated, "gateway credential secret rewritten with new content"))
	case registerUnchanged:
	}
}

func emitWarningOnConditionFalse(r *Reconciler, sc *v1beta1.SpokeCluster, prev, next *v1beta1.SpokeClusterStatus, condType string) {
	nextCond := meta.FindStatusCondition(next.Conditions, condType)
	if nextCond == nil || nextCond.Status != metav1.ConditionFalse {
		return
	}
	prevCond := (*metav1.Condition)(nil)
	if prev != nil {
		prevCond = meta.FindStatusCondition(prev.Conditions, condType)
	}
	if prevCond != nil && prevCond.Status == metav1.ConditionFalse && prevCond.Reason == nextCond.Reason {
		return
	}
	r.emit(sc, event.Warning(event.Reason(nextCond.Reason), fmt.Errorf("%s", nextCond.Message)))
	spokeConditionFailures.WithLabelValues(sc.Namespace, sc.Name, condType, nextCond.Reason).Inc()
}

func conditionMessage(status *v1beta1.SpokeClusterStatus, condType string) string {
	if status == nil {
		return ""
	}
	cond := meta.FindStatusCondition(status.Conditions, condType)
	if cond == nil || cond.Message == "" {
		return string(condType)
	}
	return cond.Message
}
