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
	"testing"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

type recordingRecorder struct {
	events []event.Event
}

func (r *recordingRecorder) Event(_ runtime.Object, e event.Event) {
	r.events = append(r.events, e)
}

func (r *recordingRecorder) WithAnnotations(_ ...string) event.Recorder { return r }

func TestEmitStatusEventsTransitions(t *testing.T) {
	rec := &recordingRecorder{}
	r := &Reconciler{record: rec}
	sc := &v1beta1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: "spoke", Namespace: "ns"}}

	prev := &v1beta1.SpokeClusterStatus{Connection: v1beta1.ConnectionStateUnknown}
	next := &v1beta1.SpokeClusterStatus{Connection: v1beta1.ConnectionStateConnected}
	setCondition(next, v1beta1.SpokeClusterConditionConnected, metav1.ConditionTrue, reasonProbeSucceeded, "spoke answered the authenticated probe")
	r.emitStatusEvents(sc, prev, next)
	if len(rec.events) != 1 || rec.events[0].Reason != reasonProbeSucceeded {
		t.Fatalf("want one ProbeSucceeded event, got %#v", rec.events)
	}

	rec.events = nil
	r.emitStatusEvents(sc, next, next)
	if len(rec.events) != 0 {
		t.Fatalf("steady Connected must not re-emit, got %#v", rec.events)
	}

	down := &v1beta1.SpokeClusterStatus{Connection: v1beta1.ConnectionStateDisconnected}
	setCondition(down, v1beta1.SpokeClusterConditionConnected, metav1.ConditionFalse, reasonProbeFailed, "timeout")
	r.emitStatusEvents(sc, next, down)
	if len(rec.events) != 1 || rec.events[0].Type != event.TypeWarning || rec.events[0].Reason != reasonProbeFailed {
		t.Fatalf("want ProbeFailed warning, got %#v", rec.events)
	}
}

func TestEmitStatusEventsConditionFailures(t *testing.T) {
	rec := &recordingRecorder{}
	r := &Reconciler{record: rec}
	sc := &v1beta1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: "spoke", Namespace: "ns"}}

	next := &v1beta1.SpokeClusterStatus{}
	setCondition(next, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionFalse, reasonMaterializeFailed, "bad kubeconfig")
	r.emitStatusEvents(sc, &v1beta1.SpokeClusterStatus{}, next)
	if len(rec.events) != 1 || string(rec.events[0].Reason) != reasonMaterializeFailed {
		t.Fatalf("want MaterializeFailed, got %#v", rec.events)
	}

	rec.events = nil
	r.emitStatusEvents(sc, next, next)
	if len(rec.events) != 0 {
		t.Fatalf("unchanged False condition must not re-emit, got %#v", rec.events)
	}
}

// Requirement 6.1. Before this, a discovery failure moved InfoSynced to False with
// nothing in `kubectl describe` to explain it.
var _ = It("EmitStatusEventsDiscoveryFailure", func() {
	resetSpokeMetrics()
	rec := &recordingRecorder{}
	r := &Reconciler{record: rec}
	sc := metricSpoke("tenant-a", "spoke-discovery")

	prev := &v1beta1.SpokeClusterStatus{Connection: v1beta1.ConnectionStateConnected}
	setCondition(prev, v1beta1.SpokeClusterConditionInfoSynced, metav1.ConditionTrue,
		reasonDiscoveryOK, "cluster inventory refreshed")
	next := prev.DeepCopy()
	setCondition(next, v1beta1.SpokeClusterConditionInfoSynced, metav1.ConditionFalse,
		reasonDiscoveryFailed, "nodes is forbidden")

	r.emitStatusEvents(sc, prev, next)
	Expect(rec.events).To(HaveLen(1))
	Expect(rec.events[0].Type).To(Equal(event.TypeWarning))
	Expect(string(rec.events[0].Reason)).To(Equal(reasonDiscoveryFailed))

	By("emitting once rather than once per probe interval", func() {
		// Requirement 6.4: a spoke whose RBAC keeps denying the node list stays quiet
		// after the first report.
		rec.events = nil
		r.emitStatusEvents(sc, next, next)
		Expect(rec.events).To(BeEmpty())
	})
})

// Requirements 6.2 and 6.3.
var _ = It("EmitGatewaySecretEvent", func() {
	rec := &recordingRecorder{}
	r := &Reconciler{record: rec}
	sc := metricSpoke("vela-system", "spoke-secret")

	r.emitGatewaySecretEvent(sc, registerCreated)
	Expect(rec.events).To(HaveLen(1))
	Expect(string(rec.events[0].Reason)).To(Equal(reasonGatewaySecretCreated))

	rec.events = nil
	r.emitGatewaySecretEvent(sc, registerUpdated)
	Expect(rec.events).To(HaveLen(1))
	Expect(string(rec.events[0].Reason)).To(Equal(reasonGatewaySecretUpdated))

	By("staying silent when the rewrite changed nothing", func() {
		// The steady state for a healthy spoke on a cached credential: identical content
		// rewritten every pass, and nothing worth telling an operator about.
		rec.events = nil
		r.emitGatewaySecretEvent(sc, registerUnchanged)
		Expect(rec.events).To(BeEmpty())
	})
})
