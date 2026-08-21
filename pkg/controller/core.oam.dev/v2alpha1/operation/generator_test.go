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
				WorkflowRunStatus: workflowv1alpha1.WorkflowRunStatus{
					Phase: workflowv1alpha1.WorkflowStateExecuting,
				},
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
