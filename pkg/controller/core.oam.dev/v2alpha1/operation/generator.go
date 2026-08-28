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

	workflowv1alpha1 "github.com/kubevela/workflow/api/v1alpha1"
	process "github.com/kubevela/workflow/pkg/cue/process"
	wfTypes "github.com/kubevela/workflow/pkg/types"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v2alpha1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/controller/core.oam.dev/v1beta1/application"
	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"
	"github.com/oam-dev/kubevela/pkg/oam"
	opoperation "github.com/oam-dev/kubevela/pkg/oam/operation"
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
// later step in the same run sees a consistent snapshot. component is the
// zero value under Application scope -- there is no single component to
// evaluate.
type resolvedTarget struct {
	app       *v1beta1.Application
	appParser *appfile.Parser
	appFile   *appfile.Appfile
	handler   *application.AppHandler
	component common.ApplicationComponent
}

// effectiveScope normalizes an OperationTemplate's attach.scope, defaulting
// the zero value to Component the same way the CRD's own
// +kubebuilder:default does at admission time -- needed because unit tests
// and any other caller that builds an OperationTemplateSpec by hand won't
// go through API server defaulting.
func effectiveScope(scope v2alpha1.OperationAttachScope) v2alpha1.OperationAttachScope {
	if scope == "" {
		return v2alpha1.OperationAttachScopeComponent
	}
	return scope
}

// resolveTarget resolves the Operation's target according to tmpl's
// attach.scope, and validates the target against the template's
// scope-specific match rules (AllowedComponentTypes / Selector) -- the
// target must be resolved before those rules can be checked, so this is
// also where "does this target match this template" is decided, not in
// resolveTemplate.
func (r *Reconciler) resolveTarget(ctx context.Context, op *v2alpha1.Operation, tmpl *v2alpha1.OperationTemplateSpec) (*resolvedTarget, error) {
	scope := effectiveScope(tmpl.Attach.Scope)

	if scope == v2alpha1.OperationAttachScopeNone {
		if op.Spec.Target != nil {
			return nil, fmt.Errorf("spec.target must be omitted for attach.scope %q", scope)
		}
		return nil, nil
	}
	if op.Spec.Target == nil {
		return nil, fmt.Errorf("spec.target is required for attach.scope %q", scope)
	}

	if op.Spec.Target.Component == nil {
		if scope != v2alpha1.OperationAttachScopeApplication {
			return nil, fmt.Errorf("operation template is %q-scoped, cannot be invoked against an Application target", scope)
		}
		target, err := r.resolveApplicationTarget(ctx, op)
		if err != nil {
			return nil, err
		}
		if tmpl.Attach.Selector != nil {
			if err := opoperation.MatchesApplicationSelector(target.app, tmpl.Attach.Selector); err != nil {
				return nil, err
			}
		}
		return target, nil
	}

	if scope != v2alpha1.OperationAttachScopeComponent {
		return nil, fmt.Errorf("operation template is %q-scoped, cannot be invoked against a Component target", scope)
	}
	target, err := r.resolveComponentTarget(ctx, op)
	if err != nil {
		return nil, err
	}
	if allowed := tmpl.Attach.AllowedComponentTypes; len(allowed) > 0 && !slices.Contains(allowed, target.component.Type) {
		return nil, fmt.Errorf("operation template does not allow component type %q (allowed: %v)", target.component.Type, allowed)
	}
	return target, nil
}

// resolveComponentTarget locates the Operation's target Component within
// its owning Application, read-only (Get/List only -- see
// PrepareCurrentAppRevision and GenerateAppFile in the upstream research
// this mirrors).
func (r *Reconciler) resolveComponentTarget(ctx context.Context, op *v2alpha1.Operation) (*resolvedTarget, error) {
	if op.Spec.Target.App == "" || *op.Spec.Target.Component == "" {
		return nil, fmt.Errorf("spec.target.app and spec.target.component are required")
	}

	app, appParser, af, err := r.getAndParseApplication(ctx, op.Namespace, op.Spec.Target.App)
	if err != nil {
		return nil, err
	}

	var comp *common.ApplicationComponent
	for i := range af.Components {
		if af.Components[i].Name == *op.Spec.Target.Component {
			comp = &af.Components[i]
			break
		}
	}
	if comp == nil {
		return nil, fmt.Errorf("component %q not found in application %q", *op.Spec.Target.Component, op.Spec.Target.App)
	}

	handler, err := r.newAppHandler(ctx, app, af)
	if err != nil {
		return nil, err
	}
	return &resolvedTarget{app: app, appParser: appParser, appFile: af, handler: handler, component: *comp}, nil
}

// resolveApplicationTarget resolves the Operation's target Application
// as a whole. There is no single component to search for -- two
// Applications sharing a template's label selector may have entirely
// different components -- so context.output/outputs/componentParams stay
// unavailable under this scope (see buildProcessContext).
func (r *Reconciler) resolveApplicationTarget(ctx context.Context, op *v2alpha1.Operation) (*resolvedTarget, error) {
	if op.Spec.Target.App == "" {
		return nil, fmt.Errorf("spec.target.app is required")
	}

	app, appParser, af, err := r.getAndParseApplication(ctx, op.Namespace, op.Spec.Target.App)
	if err != nil {
		return nil, err
	}

	handler, err := r.newAppHandler(ctx, app, af)
	if err != nil {
		return nil, err
	}
	return &resolvedTarget{app: app, appParser: appParser, appFile: af, handler: handler}, nil
}

// getAndParseApplication fetches the named Application and parses it into
// an Appfile, the shared prefix of both resolveComponentTarget and
// resolveApplicationTarget.
func (r *Reconciler) getAndParseApplication(ctx context.Context, namespace, name string) (*v1beta1.Application, *appfile.Parser, *appfile.Appfile, error) {
	app := &v1beta1.Application{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, app); err != nil {
		return nil, nil, nil, errors.Wrapf(err, "get target application %q", name)
	}

	appParser := appfile.NewApplicationParser(r.Client)
	af, err := appParser.GenerateAppFile(ctx, app)
	if err != nil {
		return nil, nil, nil, errors.Wrapf(err, "parse application %q", name)
	}
	return app, appParser, af, nil
}

// newAppHandler creates an AppHandler for app/af and prepares its current
// revision -- needed even under Application scope, since
// buildProcessContext populates context.components from af.Components.
func (r *Reconciler) newAppHandler(ctx context.Context, app *v1beta1.Application, af *appfile.Appfile) (*application.AppHandler, error) {
	// Only r.Client is read by NewAppHandler/checkComponentHealth --
	// Scheme/Recorder are irrelevant here.
	handler, err := application.NewAppHandler(ctx, &application.Reconciler{Client: r.Client}, app)
	if err != nil {
		return nil, errors.Wrap(err, "create app handler for target application")
	}
	if err := handler.PrepareCurrentAppRevision(ctx, af); err != nil {
		return nil, errors.Wrap(err, "prepare current app revision for target application")
	}
	return handler, nil
}

// resolveTemplate resolves spec.template in the Operation's own namespace
// first, then "vela-system" (the same two-tier order used for
// ComponentDefinition et al.), and validates its attach shape against
// scope-structural rules that need no resolved target (e.g. None scope
// carrying AllowedComponentTypes/Selector). Rules that need the resolved
// target (AllowedComponentTypes vs. the actual component type, Selector vs.
// the actual Application) are checked in resolveTarget instead, since the
// target isn't resolved yet when this runs -- see the Reconcile ordering.
// TODO(KEP 2.15): admission (SubjectAccessReview) isn't implemented yet.
func (r *Reconciler) resolveTemplate(ctx context.Context, op *v2alpha1.Operation) (*v2alpha1.OperationTemplate, error) {
	tmpl := &v2alpha1.OperationTemplate{}
	err := r.Get(ctx, client.ObjectKey{Namespace: op.Namespace, Name: op.Spec.Template}, tmpl)
	if kerrors.IsNotFound(err) {
		err = r.Get(ctx, client.ObjectKey{Namespace: systemDefinitionNamespace, Name: op.Spec.Template}, tmpl)
	}
	if err != nil {
		return nil, errors.Wrapf(err, "resolve operation template %q", op.Spec.Template)
	}

	switch effectiveScope(tmpl.Spec.Attach.Scope) {
	case v2alpha1.OperationAttachScopeComponent:
		// AllowedComponentTypes is checked in resolveTarget, once the
		// target's component type is known.
	case v2alpha1.OperationAttachScopeApplication:
		// Selector is checked in resolveTarget, once the target
		// Application is resolved.
	case v2alpha1.OperationAttachScopeNone:
		if len(tmpl.Spec.Attach.AllowedComponentTypes) > 0 {
			return nil, fmt.Errorf("operation template %q: allowedComponentTypes is not valid under attach.scope %q", tmpl.Name, tmpl.Spec.Attach.Scope)
		}
		if tmpl.Spec.Attach.Selector != nil {
			return nil, fmt.Errorf("operation template %q: selector is not valid under attach.scope %q", tmpl.Name, tmpl.Spec.Attach.Scope)
		}
	default:
		return nil, fmt.Errorf("operation template %q has unsupported attach.scope %q", tmpl.Name, tmpl.Spec.Attach.Scope)
	}
	return tmpl, nil
}

// buildProcessContext populates a process.Context for op's resolved
// target -- Option 1 from the KEP: a static workflow whose steps read
// `context` themselves, no `$()` expressions or deferred provider calls.
// target is nil under None scope: no OAM target, so only the
// target-independent fields (operationName, operationParams, ...) are
// populated. Under Application scope, target.component is the zero value:
// there is no single component to evaluate health for, so
// output/outputs/status stay unset -- a step referencing context.output
// under Application scope fails CUE evaluation with "field not found",
// which is how that absence is enforced (no admission-time rejection).
func buildProcessContext(ctx context.Context, op *v2alpha1.Operation, target *resolvedTarget) (process.Context, error) {
	var (
		outputObj                 interface{}
		outputsMap                map[string]interface{}
		status                    *common.ApplicationComponentStatus
		appName, compName         string
		appLabels, appAnnotations map[string]string
		components                []common.ApplicationComponent
	)

	if target != nil {
		appName = target.app.Name
		appLabels = target.app.Labels
		appAnnotations = target.app.Annotations
		components = target.appFile.Components

		if op.Spec.Target.Component == nil {
			// context.name is "the thing this workflow is about" --
			// established behavior: the Application controller's own
			// workflow sets it identically (generateContextDataFromApp).
			compName = target.app.Name
			// Deliberately NOT set: Output, Outputs, Status.
		} else {
			compName = target.component.Name
			healthCheck := target.handler.CheckComponentHealth(target.appParser, target.appFile)
			isHealthy, s, output, outputs, err := healthCheck(ctx, target.component, nil, "", "")
			if err != nil {
				return nil, errors.Wrap(err, "check target component health")
			}
			if s == nil {
				s = &common.ApplicationComponentStatus{Name: target.component.Name, Healthy: isHealthy}
			}
			status = s
			if output != nil {
				outputObj = output.Object
			}
			// oam.TraitResource carries each output's original name
			// regardless of whether it came from the component's own
			// template or a trait (see getResourceFromObj in
			// pkg/cue/definition/template.go).
			outputsMap = make(map[string]interface{}, len(outputs))
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
		}
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
		AppName:         appName,
		CompName:        compName,
		AppLabels:       appLabels,
		AppAnnotations:  appAnnotations,
		Ctx:             ctx,
		Output:          outputObj,
		Outputs:         outputsMap,
		Status:          status,
		OperationName:   op.Name,
		OperationScope:  string(effectiveScope(op.Status.Template.Attach.Scope)),
		OperationParams: params,
		StartTime:       op.Status.StartTime.Format(time.RFC3339),
		Components:      components,
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
		instance.Status = toEngineStatus(op.Status.Workflows[0])
	}
	return instance
}

// toEngineStatus converts ws to the shape the embedded engine understands,
// unwrapping each step back to its plain WorkflowStepStatus -- the engine
// has no notion of our own per-step Attempts history.
func toEngineStatus(ws v2alpha1.OperationWorkflowStatus) workflowv1alpha1.WorkflowRunStatus {
	var steps []workflowv1alpha1.WorkflowStepStatus
	if len(ws.Steps) > 0 {
		steps = make([]workflowv1alpha1.WorkflowStepStatus, len(ws.Steps))
		for i, s := range ws.Steps {
			steps[i] = s.WorkflowStepStatus
		}
	}
	return workflowv1alpha1.WorkflowRunStatus{
		Mode:           ws.Mode,
		Phase:          ws.Phase,
		Message:        ws.Message,
		Suspend:        ws.Suspend,
		SuspendState:   ws.SuspendState,
		Terminated:     ws.Terminated,
		Finished:       ws.Finished,
		ContextBackend: ws.ContextBackend,
		Steps:          steps,
		StartTime:      ws.StartTime,
		EndTime:        ws.EndTime,
	}
}

// operationWorkflowStatusFromEngine converts the embedded engine's own
// status (just returned by ExecuteRunners) back into our shape, wrapping
// each step, reattaching its Attempts history from previous -- either
// carried forward from that same step's own live entry (a step that
// stayed live across this reconcile) or, for a step a restart just
// removed and the engine is only now recreating, from previous's pending
// StepAttempts (see that field's doc on OperationWorkflowStatus) -- and
// recording this step's own completion into Attempts if it just reached a
// terminal phase (see recordStepCompletion).
func operationWorkflowStatusFromEngine(cluster string, engineStatus workflowv1alpha1.WorkflowRunStatus, previous v2alpha1.OperationWorkflowStatus) v2alpha1.OperationWorkflowStatus {
	attemptsByName := make(map[string][]v2alpha1.OperationStepAttempt, len(previous.Steps)+len(previous.StepAttempts))
	for _, s := range previous.Steps {
		if len(s.Attempts) > 0 {
			attemptsByName[s.Name] = s.Attempts
		}
	}
	for name, attempts := range previous.StepAttempts {
		attemptsByName[name] = attempts
	}

	consumed := make(map[string]bool, len(engineStatus.Steps))
	var steps []v2alpha1.OperationWorkflowStepStatus
	if len(engineStatus.Steps) > 0 {
		steps = make([]v2alpha1.OperationWorkflowStepStatus, len(engineStatus.Steps))
		for i, s := range engineStatus.Steps {
			consumed[s.Name] = true
			steps[i] = v2alpha1.OperationWorkflowStepStatus{
				WorkflowStepStatus: s,
				Attempts:           recordStepCompletion(attemptsByName[s.Name], s),
			}
		}
	}

	// op.Status.Template.Workflow.Steps is a one-time snapshot (never
	// re-resolved after the first reconcile -- see resolveTemplate), so a
	// step name that ever had a pending attempt is guaranteed to reappear
	// in engineStatus.Steps eventually -- but "eventually" isn't "this
	// reconcile": if the workflow suspends, fails, or is otherwise not
	// progressed before that happens, whatever in attemptsByName wasn't
	// just reattached above has to be carried forward here, or it's lost
	// for good the moment this return value replaces op.Status.Workflows.
	var pending map[string][]v2alpha1.OperationStepAttempt
	for name, attempts := range attemptsByName {
		if consumed[name] {
			continue
		}
		if pending == nil {
			pending = make(map[string][]v2alpha1.OperationStepAttempt, len(attemptsByName))
		}
		pending[name] = attempts
	}

	return v2alpha1.OperationWorkflowStatus{
		Cluster:        cluster,
		Mode:           engineStatus.Mode,
		Phase:          engineStatus.Phase,
		Message:        engineStatus.Message,
		Suspend:        engineStatus.Suspend,
		SuspendState:   engineStatus.SuspendState,
		Terminated:     engineStatus.Terminated,
		Finished:       engineStatus.Finished,
		ContextBackend: engineStatus.ContextBackend,
		Steps:          steps,
		StartTime:      engineStatus.StartTime,
		EndTime:        engineStatus.EndTime,
		StepAttempts:   pending,
	}
}

// recordStepCompletion appends a new attempt to attempts the moment s
// reaches a terminal phase (succeeded, failed, or skipped -- the same
// check the embedded engine itself uses, wfTypes.IsStepFinish, to decide a
// step is done), so every completed execution gets logged exactly once --
// the original run included, not only ones later superseded by a restart.
// s.ID (stable for one execution, see OperationWorkflowStepStatus's doc) is
// what makes "exactly once" possible: a step sitting in the same terminal
// phase across many reconciles has the same ID each time, so comparing
// against the last recorded attempt's ID is enough to avoid duplicates
// without any other bookkeeping.
func recordStepCompletion(attempts []v2alpha1.OperationStepAttempt, s workflowv1alpha1.WorkflowStepStatus) []v2alpha1.OperationStepAttempt {
	if !wfTypes.IsStepFinish(s.Phase, s.Reason) {
		return attempts
	}
	if last := len(attempts) - 1; last >= 0 && attempts[last].ID == s.ID {
		return attempts
	}
	return append(attempts, v2alpha1.OperationStepAttempt{
		AttemptNumber: int64(len(attempts) + 1),
		ID:            s.ID,
		Phase:         s.Phase,
		Message:       s.Message,
		Reason:        s.Reason,
		StartTime:     s.FirstExecuteTime,
	})
}
