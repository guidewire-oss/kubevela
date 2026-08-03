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

package application

import (
	"context"

	"cuelang.org/go/cue"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	veladefinition "github.com/oam-dev/kubevela/pkg/cue/definition"
	"github.com/oam-dev/kubevela/pkg/definition/sourceexpr"
	oamutil "github.com/oam-dev/kubevela/pkg/oam/util"
)

// validateExpressions checks the $(...) expressions in an Application's
// properties before it is admitted.
//
// Only what is decidable without values: the expression parses, uses no
// construct whose result type would depend on data admission has not seen, and
// reaches no identifier this surface does not offer. A mistake then surfaces
// against the property that contains it, rather than as a CUE error during a
// render that has already been accepted.
//
// Roots vary by surface. An Application-scoped policy renders before the appfile
// exists, so it has no sources to read - permitting context there and refusing
// source is what lets the surface carry expressions at all.
// appScoped reports whether a policy type is an Application-scoped
// PolicyDefinition. Supplied by the caller because that needs a client lookup,
// and this pass is otherwise a pure function of the Application.
func validateExpressions(app *v1beta1.Application, appScoped func(string) bool) field.ErrorList {
	var errs field.ErrorList

	check := func(raw *runtime.RawExtension, path *field.Path, roots ...string) {
		if raw == nil || len(raw.Raw) == 0 {
			return
		}
		var decoded interface{}
		if err := json.Unmarshal(raw.Raw, &decoded); err != nil {
			// Malformed properties are reported by the consumer's own parsing.
			return
		}
		if !sourceexpr.HasExpression(decoded) {
			return
		}
		if err := sourceexpr.ValidateTree(decoded, roots...); err != nil {
			errs = append(errs, field.Invalid(path, string(raw.Raw), err.Error()))
		}
	}

	both := []string{sourceexpr.SourceIdent, sourceexpr.ContextIdent}
	contextOnly := []string{sourceexpr.ContextIdent}

	for i, comp := range app.Spec.Components {
		p := field.NewPath("spec", "components").Index(i)
		check(comp.Properties, p.Child("properties"), both...)
		for j, tr := range comp.Traits {
			check(tr.Properties, p.Child("traits").Index(j).Child("properties"), both...)
		}
	}
	for i, src := range app.Spec.Sources {
		check(src.Properties, field.NewPath("spec", "sources").Index(i).Child("properties"), both...)
	}
	for i, policy := range app.Spec.Policies {
		// A policy with a CUE template renders through the same engine a component
		// does, so a source resolves there; a built-in one has no render at all.
		roots := contextOnly
		if veladefinition.SurfaceReadsSource(appfile.PolicySurface(policy.Type, appScoped(policy.Type))) {
			roots = both
		}
		check(policy.Properties, field.NewPath("spec", "policies").Index(i).Child("properties"), roots...)
	}
	if app.Spec.Workflow != nil {
		for i, step := range app.Spec.Workflow.Steps {
			p := field.NewPath("spec", "workflow", "steps").Index(i)
			check(step.Properties, p.Child("properties"), both...)
			for j, sub := range step.SubSteps {
				check(sub.Properties, p.Child("subSteps").Index(j).Child("properties"), both...)
			}
		}
	}

	return errs
}

// validateExpressionTargetTypes type-checks each property containing a $(...)
// expression against the parameter it feeds.
//
// This is the point of typing expressions at all: the result type is derived
// from the source schemas at admission, so a mismatch is refused before the
// Application exists rather than surfacing as a CUE error mid-render.
//
// Best-effort per target: where the target parameter type cannot be determined
// the check is skipped rather than guessed.
//
// Every surface is typed, not just components and traits. Leaving the others out
// meant a mismatch in a workflow step or a policy was caught only incidentally -
// by JSON unmarshalling, or by the dry-run render - with a message naming a Go
// field or a CUE path rather than the property the author wrote.
func (h *ValidatingHandler) validateExpressionTargetTypes(ctx context.Context, app *v1beta1.Application,
	sourceNameToType map[string]string, schemaValidators map[string]*sourceSchemaValidator,
	reported map[string]bool) field.ErrorList {
	var errs field.ErrorList
	targetParams := map[string]*cueStruct{}

	loadTarget := func(kind, defType string) *cueStruct {
		key := kind + "/" + defType
		if pv, ok := targetParams[key]; ok {
			return pv
		}
		pv, _ := h.loadTargetParameter(ctx, app.Namespace, kind, defType)
		targetParams[key] = pv
		return pv
	}

	schemasFor := h.sourceSchemaTexts(ctx, app.Namespace, sourceNameToType, schemaValidators)

	check := func(leaves []inputLeaf, param *cueStruct, targetDesc string,
		ctxSchema sourceexpr.ContextSchema, roots ...string) {
		for _, lf := range leaves {
			raw, ok := lf.literal.(string)
			if !ok || lf.path == "" {
				continue
			}
			parsed, err := sourceexpr.Parse(raw)
			if err != nil || !parsed.HasExpr() {
				continue
			}
			// The reference pass already faulted this property - an undeclared
			// source, or a path the schema does not have. Typing it would only
			// restate that in different words.
			if reported[lf.fieldPath.String()] {
				continue
			}

			srcKind, err := sourceexpr.ValueTypeIn(raw, schemasFor, ctxSchema, roots...)
			if err != nil {
				errs = append(errs, field.Invalid(lf.fieldPath, raw, err.Error()))
				continue
			}
			if param == nil {
				continue
			}
			dstKind, declared := param.kindAt(lf.path)
			if !declared {
				// The consuming template may accept it via an open struct; do not
				// over-report, exactly as the directive's check does not.
				continue
			}
			if !kindsCompatible(srcKind, dstKind) {
				errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
					fmt.Sprintf("type mismatch: expression %s is %s but %s expects %s",
						raw, kindName(srcKind), targetDesc, kindName(dstKind))))
				continue
			}

			// The same rule the directive follows: a default is required only
			// when a value that may be absent feeds a required parameter.
			undefended, uerr := sourceexpr.UndefendedReads(raw, schemasFor)
			if uerr != nil || len(undefended) == 0 {
				continue
			}
			if required, _ := param.requiredAt(lf.path); required {
				errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
					fmt.Sprintf("%s may be absent and feeds required %s; supply a default with *%s | <fallback>",
						undefended[0], targetDesc, undefended[0])))
			}
		}
	}

	both := []string{sourceexpr.SourceIdent, sourceexpr.ContextIdent}
	contextOnly := []string{sourceexpr.ContextIdent}
	for i, comp := range app.Spec.Components {
		if comp.Properties != nil && len(comp.Properties.Raw) > 0 {
			base := field.NewPath("spec", "components").Index(i).Child("properties")
			check(flattenLeafPaths(comp.Properties.Raw, base), loadTarget("component", comp.Type),
				fmt.Sprintf("component %q parameter", comp.Type), sourceexpr.ComponentContext, both...)
		}
		for j, tr := range comp.Traits {
			if tr.Properties == nil || len(tr.Properties.Raw) == 0 {
				continue
			}
			base := field.NewPath("spec", "components").Index(i).Child("traits").Index(j).Child("properties")
			check(flattenLeafPaths(tr.Properties.Raw, base), loadTarget("trait", tr.Type),
				fmt.Sprintf("trait %q parameter", tr.Type), sourceexpr.TraitContext, both...)
		}
	}

	// Policies read `context` only, so they are typed against that alone - typing
	// against a component's roots would accept a `source` this surface never
	// receives, which is the whole reason the roots vary by surface.
	for i, policy := range app.Spec.Policies {
		if policy.Properties == nil || len(policy.Properties.Raw) == 0 {
			continue
		}
		base := field.NewPath("spec", "policies").Index(i).Child("properties")
		// The policy's own context, not the component's: a policy is evaluated
		// against what its path supplies, and typing it against anything else is
		// how the two came to disagree. Which path depends on the kind - a
		// built-in policy is consumed off the appfile, a rendered one goes through
		// the engine and sees a render's context.
		scoped := h.policyIsAppScoped(ctx, app, policy.Type)
		roots := contextOnly
		if veladefinition.SurfaceReadsSource(appfile.PolicySurface(policy.Type, scoped)) {
			roots = both
		}
		check(flattenLeafPaths(policy.Properties.Raw, base), loadTarget("policy", policy.Type),
			fmt.Sprintf("policy %q parameter", policy.Type),
			appfile.PolicyContextSchema(policy.Type, scoped), roots...)
	}

	if app.Spec.Workflow != nil {
		for i, step := range app.Spec.Workflow.Steps {
			p := field.NewPath("spec", "workflow", "steps").Index(i)
			if step.Properties != nil && len(step.Properties.Raw) > 0 {
				check(flattenLeafPaths(step.Properties.Raw, p.Child("properties")),
					loadTarget("workflowstep", step.Type),
					fmt.Sprintf("workflow step %q parameter", step.Type),
					sourceexpr.WorkflowStepContext, both...)
			}
			for j, sub := range step.SubSteps {
				if sub.Properties == nil || len(sub.Properties.Raw) == 0 {
					continue
				}
				check(flattenLeafPaths(sub.Properties.Raw, p.Child("subSteps").Index(j).Child("properties")),
					loadTarget("workflowstep", sub.Type),
					fmt.Sprintf("workflow step %q parameter", sub.Type),
					sourceexpr.WorkflowStepContext, both...)
			}
		}
	}
	return errs
}

// hasSourceExpression reports whether a property value carries a $(...)
// expression.
func hasSourceExpression(raw string) bool {
	parsed, err := sourceexpr.Parse(raw)
	return err == nil && parsed.HasExpr()
}

// sourceSchemaTexts maps binding names to their SourceDefinition schema text,
// which is what sentinel typing needs.
func (h *ValidatingHandler) sourceSchemaTexts(ctx context.Context, appNamespace string,
	sourceNameToType map[string]string, schemaValidators map[string]*sourceSchemaValidator) map[string]string {
	out := map[string]string{}
	for name, sourceType := range sourceNameToType {
		if sourceType == "" {
			continue
		}
		sv, ok := schemaValidators[sourceType]
		if !ok {
			var err error
			sv, err = h.loadSourceSchemaValidator(ctx, appNamespace, sourceType)
			if err != nil {
				continue
			}
			schemaValidators[sourceType] = sv
		}
		if sv == nil || sv.schemaExpr == "" {
			continue
		}
		out[name] = sv.schemaExpr
	}
	return out
}

// expressionKind types a property value that carries expressions.
func (h *ValidatingHandler) expressionKind(ctx context.Context, appNamespace, raw string,
	sourceNameToType map[string]string, schemaValidators map[string]*sourceSchemaValidator) (cue.Kind, error) {
	return sourceexpr.ValueType(raw, h.sourceSchemaTexts(ctx, appNamespace, sourceNameToType, schemaValidators))
}

// undefendedExpressionReads returns the reads in a property value that could be
// absent at render and carry no default.
func (h *ValidatingHandler) undefendedExpressionReads(ctx context.Context, appNamespace, raw string,
	sourceNameToType map[string]string, schemaValidators map[string]*sourceSchemaValidator) []sourceexpr.Reference {
	refs, err := sourceexpr.UndefendedReads(raw, h.sourceSchemaTexts(ctx, appNamespace, sourceNameToType, schemaValidators))
	if err != nil {
		return nil
	}
	return refs
}

// policyIsAppScoped reports whether a policy type is an Application-scoped
// PolicyDefinition, which decides both the context it is typed against and
// whether it may read a source at all.
//
// A built-in type never has a definition, so it is answered without a lookup. A
// missing or unreadable definition answers false - the same fail-open every other
// surface check uses, and the definition's own absence is reported elsewhere.
func (h *ValidatingHandler) policyIsAppScoped(ctx context.Context, app *v1beta1.Application, policyType string) bool {
	if appfile.IsBuiltinPolicyType(policyType) {
		return false
	}
	def := &v1beta1.PolicyDefinition{}
	if err := oamutil.GetCapabilityDefinition(ctx, h.Client, def, policyType, app.Annotations); err != nil {
		return false
	}
	return def.Spec.Scope != v1beta1.DefaultScope
}

// policyScopeLookup returns a memoised classifier, so one Application does not
// fetch the same PolicyDefinition once per policy.
func (h *ValidatingHandler) policyScopeLookup(ctx context.Context, app *v1beta1.Application) func(string) bool {
	seen := map[string]bool{}
	return func(policyType string) bool {
		if v, ok := seen[policyType]; ok {
			return v
		}
		v := h.policyIsAppScoped(ctx, app, policyType)
		seen[policyType] = v
		return v
	}
}
