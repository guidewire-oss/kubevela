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
