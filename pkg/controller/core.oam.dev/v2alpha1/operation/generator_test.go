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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	wfv1alpha1 "github.com/kubevela/pkg/apis/oam/v1alpha1"
	workflowv1alpha1 "github.com/kubevela/workflow/api/v1alpha1"
	wfTypes "github.com/kubevela/workflow/pkg/types"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v2alpha1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	opoperation "github.com/oam-dev/kubevela/pkg/oam/operation"
	veloperation "github.com/oam-dev/kubevela/pkg/workflow/operation"
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
	tmpl, err := r.resolveTemplate(context.Background(), op)
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
	tmpl, err := r.resolveTemplate(context.Background(), op)
	require.NoError(t, err)
	assert.Equal(t, "myns", tmpl.Namespace)
}

func TestResolveTemplateNotFound(t *testing.T) {
	scheme := newTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "missing"}}
	_, err := r.resolveTemplate(context.Background(), op)
	assert.Error(t, err)
}

// TestResolveTemplateRejectsUnsupportedScope used to assert that
// Scope: "Application" was rejected -- that flipped the moment Application
// scope was implemented (see TestResolveTemplateAcceptsApplicationScope
// below). This repurposes the name for a genuinely bogus scope string.
func TestResolveTemplateRejectsUnsupportedScope(t *testing.T) {
	scheme := newTestScheme(t)
	tmpl := &v2alpha1.OperationTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: "myns"},
		Spec:       v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: "Bogus"}},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tmpl).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "restart"}}
	_, err := r.resolveTemplate(context.Background(), op)
	assert.ErrorContains(t, err, "unsupported attach.scope")
}

func TestResolveTemplateAcceptsApplicationScope(t *testing.T) {
	scheme := newTestScheme(t)
	tmpl := &v2alpha1.OperationTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "restart", Namespace: "myns"},
		Spec:       v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeApplication}},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tmpl).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "restart"}}
	got, err := r.resolveTemplate(context.Background(), op)
	require.NoError(t, err)
	assert.Equal(t, "restart", got.Name)
}

func TestResolveTemplateAcceptsNoneScope(t *testing.T) {
	scheme := newTestScheme(t)
	tmpl := &v2alpha1.OperationTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "myns"},
		Spec:       v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeNone}},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tmpl).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "bare"}}
	got, err := r.resolveTemplate(context.Background(), op)
	require.NoError(t, err)
	assert.Equal(t, "bare", got.Name)
}

func TestResolveTemplateRejectsAllowedComponentTypesUnderNoneScope(t *testing.T) {
	scheme := newTestScheme(t)
	tmpl := &v2alpha1.OperationTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "myns"},
		Spec: v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{
			Scope:                 v2alpha1.OperationAttachScopeNone,
			AllowedComponentTypes: []string{"webservice"},
		}},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tmpl).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "bare"}}
	_, err := r.resolveTemplate(context.Background(), op)
	assert.ErrorContains(t, err, "allowedComponentTypes is not valid")
}

func TestResolveTemplateRejectsSelectorUnderNoneScope(t *testing.T) {
	scheme := newTestScheme(t)
	tmpl := &v2alpha1.OperationTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "myns"},
		Spec: v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{
			Scope:    v2alpha1.OperationAttachScopeNone,
			Selector: &v2alpha1.OperationApplicationSelector{MatchLabels: map[string]string{"a": "b"}},
		}},
	}
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tmpl).Build()
	r := &Reconciler{Client: cli}

	op := &v2alpha1.Operation{ObjectMeta: metav1.ObjectMeta{Namespace: "myns"}, Spec: v2alpha1.OperationSpec{Template: "bare"}}
	_, err := r.resolveTemplate(context.Background(), op)
	assert.ErrorContains(t, err, "selector is not valid")
}

func TestResolveSourceReturnsNilForNoneScope(t *testing.T) {
	r := &Reconciler{}
	op := &v2alpha1.Operation{}
	tmpl := &v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeNone}}
	source, err := r.resolveSource(context.Background(), op, tmpl)
	require.NoError(t, err)
	assert.Nil(t, source)
}

func TestResolveSourceRejectsSourceUnderNoneScope(t *testing.T) {
	r := &Reconciler{}
	op := &v2alpha1.Operation{Spec: v2alpha1.OperationSpec{
		Source: &v2alpha1.OperationSource{App: "a", Component: ptr.To("b")},
	}}
	tmpl := &v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeNone}}
	_, err := r.resolveSource(context.Background(), op, tmpl)
	assert.ErrorContains(t, err, "must be omitted")
}

func TestResolveSourceRejectsMissingSourceUnderComponentScope(t *testing.T) {
	r := &Reconciler{}
	op := &v2alpha1.Operation{}
	tmpl := &v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeComponent}}
	_, err := r.resolveSource(context.Background(), op, tmpl)
	assert.ErrorContains(t, err, "is required")
}

func TestResolveSourceRejectsMissingSourceUnderApplicationScope(t *testing.T) {
	r := &Reconciler{}
	op := &v2alpha1.Operation{}
	tmpl := &v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeApplication}}
	_, err := r.resolveSource(context.Background(), op, tmpl)
	assert.ErrorContains(t, err, "is required")
}

func TestResolveSourceRejectsScopeSourceMismatch(t *testing.T) {
	t.Run("Component-scoped template, Application source", func(t *testing.T) {
		r := &Reconciler{}
		op := &v2alpha1.Operation{Spec: v2alpha1.OperationSpec{
			Source: &v2alpha1.OperationSource{App: "app"},
		}}
		tmpl := &v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeComponent}}
		_, err := r.resolveSource(context.Background(), op, tmpl)
		assert.ErrorContains(t, err, "cannot be invoked against an Application source")
	})

	t.Run("Application-scoped template, Component source", func(t *testing.T) {
		r := &Reconciler{}
		op := &v2alpha1.Operation{Spec: v2alpha1.OperationSpec{
			Source: &v2alpha1.OperationSource{App: "app", Component: ptr.To("comp")},
		}}
		tmpl := &v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeApplication}}
		_, err := r.resolveSource(context.Background(), op, tmpl)
		assert.ErrorContains(t, err, "cannot be invoked against a Component source")
	})
}

func TestMatchesApplicationSelector(t *testing.T) {
	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "myapp", Labels: map[string]string{"dr.oam.dev/enabled": "true", "env": "prod"}},
		Spec: v1beta1.ApplicationSpec{Components: []common.ApplicationComponent{
			{Name: "db", Type: "aurora-postgres"},
			{Name: "web", Type: "webservice"},
		}},
	}

	t.Run("MatchLabels: positive", func(t *testing.T) {
		sel := &v2alpha1.OperationApplicationSelector{MatchLabels: map[string]string{"dr.oam.dev/enabled": "true"}}
		assert.NoError(t, opoperation.MatchesApplicationSelector(app, sel))
	})
	t.Run("MatchLabels: negative", func(t *testing.T) {
		sel := &v2alpha1.OperationApplicationSelector{MatchLabels: map[string]string{"dr.oam.dev/enabled": "false"}}
		assert.Error(t, opoperation.MatchesApplicationSelector(app, sel))
	})
	t.Run("MatchExpressions: positive", func(t *testing.T) {
		sel := &v2alpha1.OperationApplicationSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"prod", "staging"}},
		}}
		assert.NoError(t, opoperation.MatchesApplicationSelector(app, sel))
	})
	t.Run("MatchExpressions: negative", func(t *testing.T) {
		sel := &v2alpha1.OperationApplicationSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
			{Key: "env", Operator: metav1.LabelSelectorOpIn, Values: []string{"dev"}},
		}}
		assert.Error(t, opoperation.MatchesApplicationSelector(app, sel))
	})
	t.Run("RequiredComponentTypes: positive", func(t *testing.T) {
		sel := &v2alpha1.OperationApplicationSelector{RequiredComponentTypes: []string{"aurora-postgres"}}
		assert.NoError(t, opoperation.MatchesApplicationSelector(app, sel))
	})
	t.Run("RequiredComponentTypes: negative", func(t *testing.T) {
		sel := &v2alpha1.OperationApplicationSelector{RequiredComponentTypes: []string{"dynamodb"}}
		assert.Error(t, opoperation.MatchesApplicationSelector(app, sel))
	})
}

func TestBuildProcessContextApplicationScopeOmitsComponentFields(t *testing.T) {
	op := &v2alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1", Namespace: "myns"},
		Spec: v2alpha1.OperationSpec{
			Source: &v2alpha1.OperationSource{App: "myapp"},
		},
		Status: v2alpha1.OperationStatus{
			Template: &v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeApplication}},
		},
	}
	source := &resolvedSource{
		app: &v1beta1.Application{ObjectMeta: metav1.ObjectMeta{Name: "myapp", Labels: map[string]string{"a": "b"}}},
		appFile: &appfile.Appfile{
			Components: []common.ApplicationComponent{{Name: "db", Type: "aurora-postgres"}},
		},
	}

	pCtx, err := buildProcessContext(context.Background(), op, source)
	require.NoError(t, err)

	base, err := pCtx.BaseContextFile()
	require.NoError(t, err)
	assert.Contains(t, base, `"appName":"myapp"`)
	assert.Contains(t, base, `"name":"myapp"`)
	assert.Contains(t, base, `"scope":"Application"`)
	assert.NotContains(t, base, `"output"`)
	assert.NotContains(t, base, `"outputs"`)
	assert.Contains(t, base, `"components"`)
}

func TestBuildProcessContextWithNilSource(t *testing.T) {
	op := &v2alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1", Namespace: "myns"},
		Status: v2alpha1.OperationStatus{
			Template:  &v2alpha1.OperationTemplateSpec{Attach: v2alpha1.OperationAttach{Scope: v2alpha1.OperationAttachScopeNone}},
			StartTime: metav1.Now(),
		},
	}

	pCtx, err := buildProcessContext(context.Background(), op, nil)
	require.NoError(t, err)

	base, err := pCtx.BaseContextFile()
	require.NoError(t, err)
	assert.Contains(t, base, `"operationName":"op-1"`)
	assert.Contains(t, base, `"scope":"None"`)
	assert.NotContains(t, base, `"output"`)
	assert.NotContains(t, base, `"outputs"`)
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

	t.Run("pending attempt not yet reattached (step hasn't reappeared this reconcile): carried forward, not dropped", func(t *testing.T) {
		// step-three's live entry was removed by a --step restart; the
		// workflow suspends (or otherwise stops) before the engine gets
		// around to recreating step-three's entry this reconcile -- its
		// pending history must survive into the returned status so a later
		// reconcile can still reattach it, instead of vanishing here.
		previous := v2alpha1.OperationWorkflowStatus{
			StepAttempts: map[string][]v2alpha1.OperationStepAttempt{
				"step-three": {{AttemptNumber: 1, ID: "id-1", Phase: workflowv1alpha1.WorkflowStepPhaseFailed}},
			},
		}
		engineStatus := workflowv1alpha1.WorkflowRunStatus{
			Suspend: true,
			Steps: []workflowv1alpha1.WorkflowStepStatus{
				{StepStatus: workflowv1alpha1.StepStatus{ID: "id-2", Name: "step-one", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
			},
		}
		got := operationWorkflowStatusFromEngine(localCluster, engineStatus, previous)
		require.Len(t, got.Steps, 1)
		assert.Equal(t, previous.StepAttempts["step-three"], got.StepAttempts["step-three"])
	})

	t.Run("pending attempt reattached this reconcile: no longer carried in StepAttempts", func(t *testing.T) {
		previous := v2alpha1.OperationWorkflowStatus{
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
		assert.NotContains(t, got.StepAttempts, "step-three", "reattached into Steps[0].Attempts, so it should not also linger in StepAttempts")
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

// TestStepRestartAttemptHistoryRoundTrip drives the full fail -> restart ->
// succeed sequence across both packages -- this controller's
// operationWorkflowStatusFromEngine (simulating what two reconciles would
// each produce) and pkg/workflow/operation's real, exported Restart (the
// same code `vela operation restart` calls) -- without a live cluster or
// the embedded CUE engine. Answers directly whether a step that fails then
// succeeds ends up with two recorded attempts, not one.
func TestStepRestartAttemptHistoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := require.New(t)
	scheme := newTestScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))
	cli := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v2alpha1.Operation{}).Build()

	steps := []wfv1alpha1.WorkflowStep{
		{WorkflowStepBase: wfv1alpha1.WorkflowStepBase{Name: "step-one", Type: "ok-step"}},
		{WorkflowStepBase: wfv1alpha1.WorkflowStepBase{Name: "step-two", Type: "ok-step"}},
		{WorkflowStepBase: wfv1alpha1.WorkflowStepBase{Name: "step-three", Type: "flaky-step"}},
	}
	op := &v2alpha1.Operation{
		ObjectMeta: metav1.ObjectMeta{Name: "flaky-step-abc", Namespace: "default"},
		Status: v2alpha1.OperationStatus{
			Template: &v2alpha1.OperationTemplateSpec{Workflow: wfv1alpha1.WorkflowSpec{Steps: steps}},
		},
	}
	r.NoError(cli.Create(ctx, op))

	// Reconcile #1: all three steps run, step-three fails for good (a real
	// terminal reason, not the still-retrying StatusReasonExecute).
	engineStatus1 := workflowv1alpha1.WorkflowRunStatus{
		Mode: wfv1alpha1.WorkflowExecuteMode{Steps: workflowv1alpha1.WorkflowModeStep},
		Steps: []workflowv1alpha1.WorkflowStepStatus{
			{StepStatus: workflowv1alpha1.StepStatus{ID: "id-1", Name: "step-one", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
			{StepStatus: workflowv1alpha1.StepStatus{ID: "id-2", Name: "step-two", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded}},
			{StepStatus: workflowv1alpha1.StepStatus{ID: "id-3", Name: "step-three", Phase: workflowv1alpha1.WorkflowStepPhaseFailed, Reason: wfTypes.StatusReasonAction, Message: "shouldFail=true"}},
		},
	}
	op.Status.Workflows = []v2alpha1.OperationWorkflowStatus{operationWorkflowStatusFromEngine(localCluster, engineStatus1, v2alpha1.OperationWorkflowStatus{})}
	op.Status.Phase = v2alpha1.OperationPhaseFailed
	now := metav1.Now()
	op.Status.CompletionTime = &now
	r.NoError(cli.Status().Update(ctx, op))

	// Sanity check on reconcile #1's own result before restarting.
	step3 := op.Status.Workflows[0].Steps[2]
	r.Len(step3.Attempts, 1, "step-three's own failure should already be recorded before any restart")
	r.Equal(workflowv1alpha1.WorkflowStepPhaseFailed, step3.Attempts[0].Phase)

	// vela operation restart flaky-step-abc --step step-three (the real,
	// exported code path -- not a hand-simulation of it).
	fetched := &v2alpha1.Operation{}
	r.NoError(cli.Get(ctx, client.ObjectKeyFromObject(op), fetched))
	r.NoError(veloperation.NewOperationWorkflowStepOperator(cli, nil, fetched).Restart(ctx, "step-three"))

	afterRestart := &v2alpha1.Operation{}
	r.NoError(cli.Get(ctx, client.ObjectKeyFromObject(op), afterRestart))
	r.Len(afterRestart.Status.Workflows[0].Steps, 2, "step-three's live entry should be gone, pending re-execution")
	r.Len(afterRestart.Status.Workflows[0].StepAttempts["step-three"], 1, "its one recorded failure should be waiting in the pending map")

	// Reconcile #2: the engine reruns just step-three (a fresh ID, since
	// its old entry was removed), and this time it succeeds.
	engineStatus2 := toEngineStatus(afterRestart.Status.Workflows[0])
	engineStatus2.Steps = append(engineStatus2.Steps, workflowv1alpha1.WorkflowStepStatus{
		StepStatus: workflowv1alpha1.StepStatus{ID: "id-4", Name: "step-three", Phase: workflowv1alpha1.WorkflowStepPhaseSucceeded},
	})
	afterRestart.Status.Workflows = []v2alpha1.OperationWorkflowStatus{operationWorkflowStatusFromEngine(localCluster, engineStatus2, afterRestart.Status.Workflows[0])}
	afterRestart.Status.Phase = v2alpha1.OperationPhaseSucceeded
	r.NoError(cli.Status().Update(ctx, afterRestart))

	final := &v2alpha1.Operation{}
	r.NoError(cli.Get(ctx, client.ObjectKeyFromObject(op), final))
	require.Len(t, final.Status.Workflows[0].Steps, 3)
	finalStep3 := final.Status.Workflows[0].Steps[2]
	assert.Equal(t, workflowv1alpha1.WorkflowStepPhaseSucceeded, finalStep3.Phase, "step-three's live status is the latest (successful) run")

	require.Len(t, finalStep3.Attempts, 2, "expected both the original failure and the successful retry logged")
	assert.Equal(t, workflowv1alpha1.WorkflowStepPhaseFailed, finalStep3.Attempts[0].Phase)
	assert.Equal(t, workflowv1alpha1.WorkflowStepPhaseSucceeded, finalStep3.Attempts[1].Phase)
	assert.EqualValues(t, 1, finalStep3.Attempts[0].AttemptNumber)
	assert.EqualValues(t, 2, finalStep3.Attempts[1].AttemptNumber)
}
