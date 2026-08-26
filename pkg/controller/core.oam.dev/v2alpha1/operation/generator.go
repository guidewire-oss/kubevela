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
	"encoding/json"
	"fmt"
	"time"

	"github.com/pkg/errors"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"k8s.io/utils/strings/slices"
	"sigs.k8s.io/controller-runtime/pkg/client"

	process "github.com/kubevela/workflow/pkg/cue/process"
	wfTypes "github.com/kubevela/workflow/pkg/types"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v2alpha1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1beta1/application"
	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"
	"github.com/oam-dev/kubevela/pkg/oam"
)

// systemDefinitionNamespace mirrors the "vela-system" fallback namespace
// used for ComponentDefinition/TraitDefinition/WorkflowStepDefinition
// resolution (two-tier: own namespace, then vela-system).
const systemDefinitionNamespace = "vela-system"

// localCluster is the only cluster resolved so far (multi-cluster dispatch
// is KEP 2.15, not yet implemented). It's always populated in status/context
// so a later multi-cluster Operation doesn't need a shape migration.
const localCluster = "local"

// resolvedTarget is everything needed to build the process context and to
// evaluate the target Component's health, gathered once per reconcile so a
// later step in the same run sees a consistent snapshot.
type resolvedTarget struct {
	app       *v1beta1.Application
	appParser *appfile.Parser
	appFile   *appfile.Appfile
	handler   *application.AppHandler
	component common.ApplicationComponent
}

// resolveTarget locates the Operation's target Component within its owning
// Application, read-only (Get/List only -- see PrepareCurrentAppRevision and
// GenerateAppFile in the upstream research this mirrors).
func (r *Reconciler) resolveTarget(ctx context.Context, op *v2alpha1.Operation) (*resolvedTarget, error) {
	if op.Spec.Target.App == "" || op.Spec.Target.Component == "" {
		return nil, fmt.Errorf("spec.target.app and spec.target.component are required")
	}

	app := &v1beta1.Application{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: op.Namespace, Name: op.Spec.Target.App}, app); err != nil {
		return nil, errors.Wrapf(err, "get target application %q", op.Spec.Target.App)
	}

	appParser := appfile.NewApplicationParser(r.Client)
	af, err := appParser.GenerateAppFile(ctx, app)
	if err != nil {
		return nil, errors.Wrapf(err, "parse application %q", op.Spec.Target.App)
	}

	var comp *common.ApplicationComponent
	for i := range af.Components {
		if af.Components[i].Name == op.Spec.Target.Component {
			comp = &af.Components[i]
			break
		}
	}
	if comp == nil {
		return nil, fmt.Errorf("component %q not found in application %q", op.Spec.Target.Component, op.Spec.Target.App)
	}

	// Only r.Client is read by NewAppHandler/checkComponentHealth --
	// Scheme/Recorder are irrelevant here.
	handler, err := application.NewAppHandler(ctx, &application.Reconciler{Client: r.Client}, app)
	if err != nil {
		return nil, errors.Wrap(err, "create app handler for target application")
	}
	if err := handler.PrepareCurrentAppRevision(ctx, af); err != nil {
		return nil, errors.Wrap(err, "prepare current app revision for target application")
	}

	return &resolvedTarget{app: app, appParser: appParser, appFile: af, handler: handler, component: *comp}, nil
}

// resolveTemplate resolves spec.template in the Operation's own namespace
// first, then "vela-system" (the same two-tier order used for
// ComponentDefinition et al.), and validates it against the target's
// component type.
// TODO(KEP 2.15): admission (SubjectAccessReview) isn't implemented yet.
func (r *Reconciler) resolveTemplate(ctx context.Context, op *v2alpha1.Operation, componentType string) (*v2alpha1.OperationTemplate, error) {
	tmpl := &v2alpha1.OperationTemplate{}
	err := r.Get(ctx, client.ObjectKey{Namespace: op.Namespace, Name: op.Spec.Template}, tmpl)
	if kerrors.IsNotFound(err) {
		err = r.Get(ctx, client.ObjectKey{Namespace: systemDefinitionNamespace, Name: op.Spec.Template}, tmpl)
	}
	if err != nil {
		return nil, errors.Wrapf(err, "resolve operation template %q", op.Spec.Template)
	}

	if tmpl.Spec.Attach.Scope != "" && tmpl.Spec.Attach.Scope != v2alpha1.OperationAttachScopeComponent {
		return nil, fmt.Errorf("operation template %q has unsupported attach.scope %q: only %q is supported so far", tmpl.Name, tmpl.Spec.Attach.Scope, v2alpha1.OperationAttachScopeComponent)
	}
	if len(tmpl.Spec.Attach.AllowedComponentTypes) > 0 && !slices.Contains(tmpl.Spec.Attach.AllowedComponentTypes, componentType) {
		return nil, fmt.Errorf("operation template %q does not allow component type %q (allowed: %v)", tmpl.Name, componentType, tmpl.Spec.Attach.AllowedComponentTypes)
	}
	return tmpl, nil
}

// buildProcessContext evaluates the target Component's health through the
// same path a healthPolicy already uses (AppHandler.CheckComponentHealth),
// then populates a process.Context from it -- Option 1 from the KEP: a
// static workflow whose steps read `context` themselves, no `$()`
// expressions or deferred provider calls.
func buildProcessContext(ctx context.Context, op *v2alpha1.Operation, target *resolvedTarget) (process.Context, error) {
	healthCheck := target.handler.CheckComponentHealth(target.appParser, target.appFile)
	isHealthy, status, output, outputs, err := healthCheck(ctx, target.component, nil, "", "")
	if err != nil {
		return nil, errors.Wrap(err, "check target component health")
	}
	if status == nil {
		status = &common.ApplicationComponentStatus{Name: target.component.Name, Healthy: isHealthy}
	}

	var outputObj interface{}
	if output != nil {
		outputObj = output.Object
	}
	// oam.TraitResource carries each output's original name regardless of
	// whether it came from the component's own template or a trait (see
	// getResourceFromObj in pkg/cue/definition/template.go).
	outputsMap := make(map[string]interface{}, len(outputs))
	for _, o := range outputs {
		if o == nil {
			continue
		}
		key := o.GetLabels()[oam.TraitResource]
		if key == "" {
			key = o.GetName()
		}
		outputsMap[key] = o.Object
	}

	var params map[string]interface{}
	if op.Spec.Parameters != nil && len(op.Spec.Parameters.Raw) > 0 {
		if err := json.Unmarshal(op.Spec.Parameters.Raw, &params); err != nil {
			return nil, errors.Wrap(err, "unmarshal spec.parameters")
		}
	}

	data := velaprocess.ContextData{
		Namespace:       op.Namespace,
		Cluster:         localCluster,
		AppName:         target.app.Name,
		CompName:        target.component.Name,
		AppLabels:       target.app.Labels,
		AppAnnotations:  target.app.Annotations,
		Ctx:             ctx,
		Output:          outputObj,
		Outputs:         outputsMap,
		Status:          status,
		OperationName:   op.Name,
		OperationScope:  string(v2alpha1.OperationAttachScopeComponent),
		OperationParams: params,
		StartTime:       op.Status.StartTime.Format(time.RFC3339),
	}
	return velaprocess.NewContext(data), nil
}

// buildWorkflowInstance builds the WorkflowInstance to run from the
// Operation's resolved (snapshotted) template, carrying forward any
// in-progress WorkflowRunStatus from a previous reconcile of the same
// Operation so the embedded engine can resume mid-workflow rather than
// restart every reconcile.
func buildWorkflowInstance(op *v2alpha1.Operation) *wfTypes.WorkflowInstance {
	instance := &wfTypes.WorkflowInstance{
		WorkflowMeta: wfTypes.WorkflowMeta{
			Name:        op.Name,
			Namespace:   op.Namespace,
			Annotations: op.Annotations,
			Labels:      op.Labels,
			UID:         op.UID,
			ChildOwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: v2alpha1.SchemeGroupVersion.String(),
					Kind:       v2alpha1.OperationKind,
					Name:       op.Name,
					UID:        op.UID,
					Controller: ptr.To(true),
				},
			},
		},
		Steps: op.Status.Template.Workflow.Steps,
	}
	if len(op.Status.Workflows) > 0 {
		instance.Status = op.Status.Workflows[0].WorkflowRunStatus
	}
	return instance
}

// carryForwardStepAttempts returns op's currently-persisted StepAttempts, so
// Reconcile can thread it through its rebuild of op.Status.Workflows.
// StepAttempts is populated by a CLI-triggered restart
// (pkg/workflow/operation), not by Reconcile itself -- without this, the
// literal `op.Status.Workflows = []v2alpha1.OperationWorkflowStatus{{...}}`
// Reconcile does after every execution would silently wipe it back to nil,
// since that literal only sets Cluster/WorkflowRunStatus.
func carryForwardStepAttempts(op *v2alpha1.Operation) map[string][]v2alpha1.OperationStepAttempt {
	if len(op.Status.Workflows) == 0 {
		return nil
	}
	return op.Status.Workflows[0].StepAttempts
}
