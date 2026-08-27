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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	wfv1alpha1 "github.com/kubevela/pkg/apis/oam/v1alpha1"
	workflowv1alpha1 "github.com/kubevela/workflow/api/v1alpha1"
	wfTypes "github.com/kubevela/workflow/pkg/types"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v2alpha1"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v2alpha1.AddToScheme(scheme))
	return scheme
}

func TestResolveTemplateNamespaceFallback(t *testing.T) {
	scheme := newTestScheme(t)
	systemTmpl := &v2alpha1.OperationTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: systemDefinitionNamespace},
		Spec:       v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeComponent}},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(systemTmpl).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "restart"}}
	tmpl, err := r.resolveTemplate(context.Background(), op, "webservice")
	require.NoError(t, err)
	assert.Equal(t, systemDefinitionNamespace, tmpl.Namespace)
}

func TestResolveTemplatePrefersOwnNamespace(t *testing.T) {
	scheme := newTestScheme(t)
	ownTmpl := &v2alpha1.OperationTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: "myns"},
		Spec:       v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeComponent}},
	}
	systemTmpl := &v2alpha1.OperationTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: systemDefinitionNamespace},
		Spec:       v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeComponent}},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ownTmpl, systemTmpl).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "restart"}}
	tmpl, err := r.resolveTemplate(context.Background(), op, "webservice")
	require.NoError(t, err)
	assert.Equal(t, "myns", tmpl.Namespace)
}

func TestResolveTemplateNotFound(t *testing.T) {
	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "missing"}}
	_, err := r.resolveTemplate(context.Background(), op, "webservice")
	assert.Error(t, err)
}

func TestResolveTemplateRejectsUnsupportedScope(t *testing.T) {
	scheme := newTestScheme(t)
	tmpl := &v2alpha1.OperationTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: "myns"},
		Spec:       v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: "Application"}},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tmpl).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "restart"}}
	_, err := r.resolveTemplate(context.Background(), op, "webservice")
	assert.ErrorContains(t, err, "unsupported attach.scope")
}

func TestResolveTemplateRejectsDisallowedComponentType(t *testing.T) {
	scheme := newTestScheme(t)
	tmpl := &v2alpha1.OperationTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: "myns"},
		Spec: v2alpha1.OperationTemplateSpec{
			Attach: v2alpha1.OperationAttach{
				Scope:                 v2alpha1.OperationAttachScopeComponent,
				AllowedComponentTypes: []string{"webservice"},
			},
		},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tmpl).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "restart"}}
	_, err := r.resolveTemplate(context.Background(), op, "worker")
	assert.ErrorContains(t, err, "does not allow component type")

	tmpl2, err := r.resolveTemplate(context.Background(), op, "webservice")
	require.NoError(t, err)
	assert.Equal(t, "restart", tmpl2.Name)
}

func TestBuildWorkflowInstanceCarriesForwardPreviousStatus(t *testing.T) {
	op := &v2alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: "restart-1", Namespace: "myns", UID: "uid-1"},
		Status: v2alpha1.OperationStatus{
			Template: &v2alpha1.OperationTemplateSpec{
				Workflow: wfv1alpha1.WorkflowSpec{
					Steps: []wfv1alpha1.WorkflowStep{{WorkflowStepBase: wfv1alpha1.WorkflowStepBase{Name: "step1", Type: "suspend"}}},
				},
			},
			Workflows: []v2alpha1.OperationWorkflowStatus{{
				Cluster: localCluster,
				Phase:   workflowv1alpha1.WorkflowStateExecuting,
			}},
		},
	}

	instance := buildWorkflowInstance(op)
	assert.Equal(t, "restart-1", instance.Name)
	assert.Equal(t, "myns", instance.Namespace)
	require.Len(t, instance.Steps, 1)
	assert.Equal(t, "step1", instance.Steps[0].Name)
	assert.Equal(t, workflowv1alpha1.WorkflowStateExecuting, instance.Status.Phase)
	require.Len(t, instance.ChildOwnerReferences, 1)
	assert.Equal(t, v2alpha1.OperationKind, instance.ChildOwnerReferences[0].Kind)
}

// TestOperationWorkflowStatusFromEngineCarriesForwardAttempts is a
// regression test: Reconcile's rebuild of op.Status.Workflows after every
// execution used to silently drop per-step attempt history, since the old
// literal only ever set Cluster/WorkflowRunStatus. Confirmed live: a
// --step restart's attempt record never survived the very next reconcile.
func TestOperationWorkflowStatusFromEngineCarriesForwardAttempts(t *testing.T) {
	t.Run("step already recorded (same execution ID): not duplicated across reconciles", func(t *testing.T) {
		previous := v2alpha1.OperationWorkflowStatus{
			Steps: []v2alpha1.OperationWorkflowStepStatus{
				{
					WorkflowStepStatus: workflowv1alpha1.WorkflowStepStatus{StepStatus: workflowv1alpha1.StepStatus{ID: "id-1", Name: "step-one", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
					Attempts:           []v2alpha1.OperationStepAttempt{{AttemptNumber: 1, ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
				},
			},
		}
		// Same step, same execution ID, still Succeeded -- a later reconcile
		// re-observing a step that's already finished, not a new attempt.
		engineStatus := workflowv1alpha1.WorkflowRunStatus{
			Steps: []workflowv1alpha1.WorkflowStepStatus{
				{StepStatus: workflowv1alpha1.StepStatus{ID: "id-1", Name: "step-one", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
			},
		}
		got := operationWorkflowStatusFromEngine(localCluster, engineStatus, previous)
		require.Len(t, got.Steps, 1)
		assert.Equal(t, previous.Steps[0].Attempts, got.Steps[0].Attempts)
	})

	t.Run("step just reached a terminal phase for the first time: recorded even with no restart involved", func(t *testing.T) {
		previous := v2alpha1.OperationWorkflowStatus{
			Steps: []v2alpha1.OperationWorkflowStepStatus{
				{WorkflowStepStatus: workflowv1alpha1.WorkflowStepStatus{StepStatus: workflowv1alpha1.StepStatus{ID: "id-1", Name: "step-one", Phase: workflowv1alpha1.WorkflowStepPhaseRunning}}},
			},
		}
		engineStatus := workflowv1alpha1.WorkflowRunStatus{
			Steps: []workflowv1alpha1.WorkflowStepStatus{
				{StepStatus: workflowv1alpha1.StepStatus{ID: "id-1", Name: "step-one", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
			},
		}
		got := operationWorkflowStatusFromEngine(localCluster, engineStatus, previous)
		require.Len(t, got.Steps, 1)
		assert.Equal(t, []v2alpha1.OperationStepAttempt{{AttemptNumber: 1, ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}}, got.Steps[0].Attempts)
	})

	t.Run("step just recreated after a --step restart: pending attempt reattached, then this new execution recorded too", func(t *testing.T) {
		previous := v2alpha1.OperationWorkflowStatus{
			// step-three's live entry was removed by the restart; its
			// history is waiting in StepAttempts until the engine
			// recreates the entry (this reconcile), with a fresh ID.
			StepAttempts: map[string][]v2alpha1.OperationStepAttempt{
				"step-three": {{AttemptNumber: 1, ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseFailed}},
			},
		}
		engineStatus := workflowv1alpha1.WorkflowRunStatus{
			Steps: []workflowv1alpha1.WorkflowStepStatus{
				{StepStatus: workflowv1alpha1.StepStatus{ID: "id-2", Name: "step-three", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
			},
		}
		got := operationWorkflowStatusFromEngine(localCluster, engineStatus, previous)
		require.Len(t, got.Steps, 1)
		assert.Equal(t, []v2alpha1.OperationStepAttempt{
			{AttemptNumber: 1, ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseFailed},
			{AttemptNumber: 2, ID: "id-2", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded},
		}, got.Steps[0].Attempts)
	})

	t.Run("no prior attempts: nil, not an empty slice", func(t *testing.T) {
		engineStatus := workflowv1alpha1.WorkflowRunStatus{
			Steps: []workflowv1alpha1.WorkflowStepStatus{
				{StepStatus: workflowv1alpha1.StepStatus{Name: "step-one", Phase: workflowv1alpha1.WorkflowStepPhaseRunning}},
			},
		}
		got := operationWorkflowStatusFromEngine(localCluster, engineStatus, v2alpha1.OperationWorkflowStatus{})
		require.Len(t, got.Steps, 1)
		assert.Nil(t, got.Steps[0].Attempts)
	})
}

func TestRecordStepCompletion(t *testing.T) {
	t.Run("running: not recorded", func(t *testing.T) {
		s := workflowv1alpha1.WorkflowStepStatus{StepStatus: workflowv1alpha1.StepStatus{ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseRunning}}
		assert.Nil(t, recordStepCompletion(nil, s))
	})

	t.Run("failed but still internally retrying (reason Execute): not yet a finished attempt", func(t *testing.T) {
		s := workflowv1alpha1.WorkflowStepStatus{StepStatus: workflowv1alpha1.StepStatus{ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseFailed, Reason: wfTypes.StatusReasonExecute}}
		assert.Nil(t, recordStepCompletion(nil, s))
	})

	t.Run("failed for good (a real terminal reason): recorded", func(t *testing.T) {
		s := workflowv1alpha1.WorkflowStepStatus{StepStatus: workflowv1alpha1.StepStatus{ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseFailed, Reason: wfTypes.StatusReasonFailedAfterRetries, Message: "boom"}}
		got := recordStepCompletion(nil, s)
		require.Len(t, got, 1)
		assert.Equal(t, v2alpha1.OperationStepAttempt{AttemptNumber: 1, ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseFailed, Reason: wfTypes.StatusReasonFailedAfterRetries, Message: "boom"}, got[0])
	})

	t.Run("skipped: recorded", func(t *testing.T) {
		s := workflowv1alpha1.WorkflowStepStatus{StepStatus: workflowv1alpha1.StepStatus{ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseSkipped}}
		got := recordStepCompletion(nil, s)
		require.Len(t, got, 1)
		assert.EqualValues(t, 1, got[0].AttemptNumber)
	})

	t.Run("same ID as the last recorded attempt: not duplicated", func(t *testing.T) {
		existing := []v2alpha1.OperationStepAttempt{{AttemptNumber: 1, ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}}
		s := workflowv1alpha1.WorkflowStepStatus{StepStatus: workflowv1alpha1.StepStatus{ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}}
		assert.Equal(t, existing, recordStepCompletion(existing, s))
	})

	t.Run("different ID: appended as the next attempt number", func(t *testing.T) {
		existing := []v2alpha1.OperationStepAttempt{{AttemptNumber: 1, ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseFailed}}
		s := workflowv1alpha1.WorkflowStepStatus{StepStatus: workflowv1alpha1.StepStatus{ID: "id-2", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}}
		got := recordStepCompletion(existing, s)
		require.Len(t, got, 2)
		assert.EqualValues(t, 2, got[1].AttemptNumber)
		assert.Equal(t, "id-2", got[1].ID)
	})
}

func TestToEngineStatusUnwrapsSteps(t *testing.T) {
	ws := v2alpha1.OperationWorkflowStatus{
		Terminated: true,
		Steps: []v2alpha1.OperationWorkflowStepStatus{
			{
				WorkflowStepStatus: workflowv1alpha1.WorkflowStepStatus{StepStatus: workflowv1alpha1.StepStatus{Name: "step-one", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
				Attempts:           []v2alpha1.OperationStepAttempt{{AttemptNumber: 1}},
			},
		},
	}
	got := toEngineStatus(ws)
	assert.True(t, got.Terminated)
	require.Len(t, got.Steps, 1)
	assert.Equal(t, "step-one", got.Steps[0].Name)
}
