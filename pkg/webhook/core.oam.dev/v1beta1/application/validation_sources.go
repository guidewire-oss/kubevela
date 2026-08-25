package application

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/oam-dev/kubevela/pkg/definition/celexpr"
	"github.com/oam-dev/kubevela/pkg/sources"

	"cuelang.org/go/cue"
	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"

	utilfeature "k8s.io/apiserver/pkg/util/feature"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"

	"github.com/oam-dev/kubevela/pkg/definition/cachekey"
	"github.com/oam-dev/kubevela/pkg/features"
	"github.com/oam-dev/kubevela/pkg/oam"
	"github.com/oam-dev/kubevela/pkg/webhook/core.oam.dev/v1beta1/sourcedefinition"
)

// ValidateSources validates source bindings and the source reads expressions make.
func (h *ValidatingHandler) ValidateSources(ctx context.Context, app *v1beta1.Application) field.ErrorList {
	var errs field.ErrorList

	// Nothing here runs unless expressions are enabled for this Application. The
	// same decision the render makes, from the same function, because an
	// Application admitted under one answer and rendered under the other is the
	// one outcome worse than either.
	if !sources.ExpressionsEnabledFor(app.GetAnnotations()) {
		// Declaring sources without them enabled is refused rather than ignored.
		// Ignoring would render $(source.x.y) into the workload as text, which
		// reaches the cluster looking like a value and fails much further away.
		if len(app.Spec.Sources) > 0 {
			errs = append(errs, field.Forbidden(field.NewPath("spec", "sources"),
				sourcesDisabledMessage()))
		}
		return errs
	}

	// Expression syntax and sandbox first: it needs no definition lookups, so a
	// typo is reported even when the rest of validation cannot run.
	appScoped := h.policyScopeLookup(ctx, app)
	errs = append(errs, validateExpressions(app, appScoped)...)

	sourceNameToType := map[string]string{}
	sourceNameToIndex := map[string]int{}
	for i, src := range app.Spec.Sources {
		p := field.NewPath("spec", "sources").Index(i)
		if src.Name == "" {
			errs = append(errs, field.Required(p.Child("name"), "source name is required"))
			continue
		}
		if prev, ok := sourceNameToIndex[src.Name]; ok {
			errs = append(errs, field.Invalid(p.Child("name"), src.Name, fmt.Sprintf("duplicated source name, already defined at index %d", prev)))
			continue
		}
		sourceNameToIndex[src.Name] = i
		sourceNameToType[src.Name] = src.Type
	}

	var refs []sourceReference
	for i, comp := range app.Spec.Components {
		compRefs, refErrs := collectSourceRefs(comp.Properties, field.NewPath("spec", "components").Index(i).Child("properties"), -1)
		errs = append(errs, refErrs...)
		refs = append(refs, withSurface(compRefs, sources.SurfaceComponent)...)
		for j, tr := range comp.Traits {
			trRefs, trErrs := collectSourceRefs(tr.Properties, field.NewPath("spec", "components").Index(i).Child("traits").Index(j).Child("properties"), -1)
			errs = append(errs, trErrs...)
			refs = append(refs, withSurface(trRefs, sources.SurfaceTrait)...)
		}
	}
	for i, policy := range app.Spec.Policies {
		policyRefs, policyErrs := collectSourceRefs(policy.Properties, field.NewPath("spec", "policies").Index(i).Child("properties"), -1)
		errs = append(errs, policyErrs...)
		refs = append(refs, withSurface(policyRefs, appfile.PolicySurface(policy.Type, appScoped(policy.Type)))...)
	}
	if app.Spec.Workflow != nil {
		for i, step := range app.Spec.Workflow.Steps {
			stepRefs, stepErrs := collectSourceRefs(step.Properties, field.NewPath("spec", "workflow", "steps").Index(i).Child("properties"), -1)
			errs = append(errs, stepErrs...)
			refs = append(refs, withSurface(stepRefs, sources.SurfaceWorkflowStep)...)
			for j, sub := range step.SubSteps {
				subRefs, subErrs := collectSourceRefs(sub.Properties, field.NewPath("spec", "workflow", "steps").Index(i).Child("subSteps").Index(j).Child("properties"), -1)
				errs = append(errs, subErrs...)
				refs = append(refs, withSurface(subRefs, sources.SurfaceWorkflowStep)...)
			}
		}
	}
	for i, src := range app.Spec.Sources {
		srcRefs, srcErrs := collectSourceRefs(src.Properties, field.NewPath("spec", "sources").Index(i).Child("properties"), i)
		errs = append(errs, srcErrs...)
		refs = append(refs, withSurface(srcRefs, sources.SurfaceSource)...)
	}

	schemaValidators := map[string]*sourceSchemaValidator{}
	consumableFromCache := map[string][]string{}
	requiredContextCache := map[string][]string{}
	// Which surfaces each binding actually resolves on, chains followed.
	bindingAt := map[int]string{}
	for name, idx := range sourceNameToIndex {
		bindingAt[idx] = name
	}
	effective := effectiveSurfaces(refs, bindingAt)
	// Field paths this pass has already faulted. The type pass below reaches the
	// same properties by a different route and would otherwise restate an
	// undeclared source or an unknown schema path in its own words.
	reported := map[string]bool{}
	fault := func(ref sourceReference, value interface{}, msg string) {
		errs = append(errs, field.Invalid(ref.FieldPath, value, msg))
		reported[ref.FieldPath.String()] = true
	}
	for _, ref := range refs {
		sourceType, ok := sourceNameToType[ref.SourceName]
		if !ok {
			fault(ref, ref.SourceName, "source is not declared in spec.sources")
			continue
		}
		if ref.SourceIndex >= 0 {
			depIdx, exists := sourceNameToIndex[ref.SourceName]
			if !exists {
				fault(ref, ref.SourceName, "source is not declared in spec.sources")
				continue
			}
			if depIdx >= ref.SourceIndex {
				fault(ref, ref.SourceName,
					fmt.Sprintf("source at index %d can only depend on prior sources, but %q is at index %d", ref.SourceIndex, ref.SourceName, depIdx))
				continue
			}
		}
		// No surface check here: validateExpressions above already restricts
		// which roots each surface offers, and reading `source` where it cannot
		// resolve is exactly what that refuses. Checking it again produced two
		// errors for one mistake.
		if sourceType == "" {
			continue
		}
		// A SourceDefinition may restrict where it can be consumed from.
		if ref.Surface == sources.SurfaceComponent || ref.Surface == sources.SurfaceTrait {
			surfaces, err := h.loadConsumableFrom(ctx, app.Namespace, sourceType, consumableFromCache, app.GetAnnotations())
			if err != nil {
				errs = append(errs, field.Invalid(ref.FieldPath, ref.Path,
					fmt.Sprintf("failed to load SourceDefinition %q: %v", sourceType, err)))
				continue
			}
			if !sourcedefinition.SurfaceAllowed(surfaces, ref.Surface) {
				errs = append(errs, field.Invalid(ref.FieldPath, ref.Path,
					fmt.Sprintf("SourceDefinition %q declares consumableFrom %v and cannot be consumed from a %s", sourceType, surfaces, ref.Surface)))
				continue
			}
		}

		// A source resolves in its call site's context, so it can only be consumed
		// where every field its template reads exists.
		//
		// Which surfaces that means depends on how this reference reaches the
		// source. A direct read resolves right here, at this one call site, so
		// only this surface has to satisfy it - a per-component source consumed by
		// a component is fine even when the same binding is also read from a
		// workflow step, and it is that second read alone that is wrong. A chained
		// read resolves inside whichever render triggered the outer binding, so it
		// must satisfy the outer binding's consumers - which is what
		// effectiveSurfaces works out.
		required, rerr := h.requiredContext(ctx, app.Namespace, sourceType, app.GetAnnotations(), requiredContextCache)
		if rerr == nil && len(required) > 0 {
			mustSatisfy := []string{ref.Surface}
			if ref.SourceIndex >= 0 {
				mustSatisfy = effective[ref.SourceName]
			}
			for _, surface := range mustSatisfy {
				if cerr := cachekey.CheckSurface(required, surface); cerr != nil {
					fault(ref, ref.Path, fmt.Sprintf("SourceDefinition %q %v", sourceType, cerr))
					break
				}
			}
		}
		validator, exists := schemaValidators[sourceType]
		if !exists {
			var err error
			validator, err = h.loadSourceSchemaValidator(ctx, app.Namespace, sourceType, app.GetAnnotations())
			if err != nil {
				errs = append(errs, field.Invalid(ref.FieldPath, ref.Path, fmt.Sprintf("failed to load SourceDefinition %q schema: %v", sourceType, err)))
				continue
			}
			schemaValidators[sourceType] = validator
		}
		if validator == nil {
			continue
		}
		if !ref.OpaquePath && !validator.HasPath(ref.Path) {
			fault(ref, ref.Path,
				fmt.Sprintf("path %q is not declared in schema of SourceDefinition %q", ref.Path, sourceType))
			continue
		}
		// The "optional source field consumed without a default" check is
		// target-aware (KEP: a default is required only when the optional field
		// feeds a REQUIRED target parameter). It is enforced in the target-aware
		// passes below (validateSourceInputs for source-property targets,
		// validateExpressionTargetTypes for component/trait targets), which know
		// the target parameter's optional/required marker.
	}

	// A source's properties are evaluated in the *consumer's* context, so a
	// context read there must exist on every surface that consumes the binding.
	errs = append(errs, validateSourceContextReads(app, effective)...)

	// Input contract: validate each source's properties against that
	// SourceDefinition's parameter: block (unknown fields + type compatibility).
	errs = append(errs, h.validateSourceInputs(ctx, app, sourceNameToType, schemaValidators)...)

	// Target contract: each expression's result type must be compatible with the
	// consuming component/trait parameter it is substituted into.
	errs = append(errs, h.validateExpressionTargetTypes(ctx, app, sourceNameToType, schemaValidators, reported)...)

	return errs
}

// validateSourceInputs checks that every source binding's properties conform to
// the referenced SourceDefinition's parameter: block: no undeclared fields, and
// each provided value's type is compatible with the declared parameter type.
// Values fed by an expression take their type from the referenced source's schema:
// output field, so a chained value's type is checked without resolving it.
func (h *ValidatingHandler) validateSourceInputs(ctx context.Context, app *v1beta1.Application, sourceNameToType map[string]string, schemaValidators map[string]*sourceSchemaValidator) field.ErrorList {
	var errs field.ErrorList
	paramValidators := map[string]*cueStruct{}
	for i, src := range app.Spec.Sources {
		if src.Type == "" || src.Properties == nil || len(src.Properties.Raw) == 0 {
			continue
		}
		basePath := field.NewPath("spec", "sources").Index(i).Child("properties")
		pv, cached := paramValidators[src.Type]
		if !cached {
			var err error
			pv, err = h.loadSourceParameter(ctx, app.Namespace, src.Type, app.GetAnnotations())
			if err != nil {
				errs = append(errs, field.Invalid(basePath, src.Type, fmt.Sprintf("failed to load SourceDefinition %q parameter schema: %v", src.Type, err)))
				paramValidators[src.Type] = nil
				continue
			}
			paramValidators[src.Type] = pv
		}
		if pv == nil {
			// Definition declares no parameter block; any provided property is
			// undeclared. Only flag when properties are actually supplied.
			leaves := flattenLeafPaths(src.Properties.Raw, basePath)
			for _, lf := range leaves {
				errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
					fmt.Sprintf("SourceDefinition %q declares no parameters, but property %q was supplied", src.Type, lf.path)))
			}
			continue
		}
		for _, lf := range flattenLeafPaths(src.Properties.Raw, basePath) {
			errs = append(errs, h.checkInputLeaf(lf, pv, src.Type, sourceNameToType, schemaValidators, ctx, app.Namespace, app.GetAnnotations())...)
		}
	}
	return errs
}

// inputLeaf is a single scalar value within a properties blob, addressed by its
// dotted path relative to the parameter block.
type inputLeaf struct {
	path      string      // dotted path into the parameter block, e.g. "region"
	fieldPath *field.Path // full field path for error reporting
	literal   interface{} // the scalar value at this path
}

// flattenLeafPaths walks a properties JSON blob and returns one inputLeaf per
// scalar node, keyed by dotted path. Array elements are addressed by index.
// Returns nothing on unparseable input, which the collection pass reports.
func flattenLeafPaths(raw []byte, basePath *field.Path) []inputLeaf {
	var decoded interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	var out []inputLeaf
	var walk func(node interface{}, dotted string, fp *field.Path)
	walk = func(node interface{}, dotted string, fp *field.Path) {
		switch v := node.(type) {
		case map[string]interface{}:
			for k, child := range v {
				next := k
				if dotted != "" {
					next = dotted + "." + k
				}
				walk(child, next, fp.Child(k))
			}
		case []interface{}:
			for idx, child := range v {
				walk(child, fmt.Sprintf("%s.%d", dotted, idx), fp.Index(idx))
			}
		default:
			out = append(out, inputLeaf{path: dotted, fieldPath: fp, literal: node})
		}
	}
	walk(decoded, "", basePath)
	return out
}

// jsonKind maps a decoded JSON scalar to the CUE kind it would satisfy.
func jsonKind(v interface{}) cue.Kind {
	switch n := v.(type) {
	case string:
		return cue.StringKind
	case bool:
		return cue.BoolKind
	case float64:
		// JSON numbers decode to float64; treat integral values as int-compatible.
		if n == float64(int64(n)) {
			return cue.IntKind
		}
		return cue.NumberKind
	case nil:
		return cue.NullKind
	}
	return cue.BottomKind
}

// checkInputLeaf validates one properties leaf against the target parameter
// block: the field must be declared, and its type must be compatible with the
// declared parameter type. Expression-fed leaves take their type from the
// referenced source's schema output field.
func (h *ValidatingHandler) checkInputLeaf(lf inputLeaf, param *cueStruct, sourceType string, sourceNameToType map[string]string, schemaValidators map[string]*sourceSchemaValidator, ctx context.Context, appNamespace string, annotations map[string]string) field.ErrorList {
	var errs field.ErrorList
	if lf.path == "" {
		return errs
	}
	dstKind, declared := param.kindAt(lf.path)
	if !declared {
		errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
			fmt.Sprintf("property %q is not declared in the parameter schema of SourceDefinition %q", lf.path, sourceType)))
		return errs
	}
	// Determine the incoming value's type.
	var srcKind cue.Kind
	var srcType *cel.Type
	if raw, isString := lf.literal.(string); isString && hasSourceExpression(raw) {
		// A source's own properties may be fed by an expression - that is how
		// chaining is written without the directive. Typing it as the string it
		// literally is would reject every non-string target.
		k, kt, terr := h.expressionKind(ctx, annotations, appNamespace, raw, sourceNameToType, schemaValidators)
		if terr != nil {
			errs = append(errs, field.Invalid(lf.fieldPath, raw, terr.Error()))
			return errs
		}
		srcKind, srcType = k, kt

		// The same optional-feeds-required rule the directive follows.
		if undefended := h.undefendedExpressionReads(ctx, annotations, appNamespace, raw, sourceNameToType, schemaValidators); len(undefended) > 0 {
			if param.requiredAt(lf.path) {
				errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
					fmt.Sprintf("%s may be absent and feeds required parameter %q of SourceDefinition %q; guard it with has(%s) ? %s : <fallback>",
						undefended[0], lf.path, sourceType, undefended[0], undefended[0])))
			}
		}
	} else {
		srcKind = jsonKind(lf.literal)
	}
	if !kindsCompatible(srcKind, dstKind) {
		errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
			fmt.Sprintf("type mismatch for parameter %q of SourceDefinition %q: expected %s, got %s",
				lf.path, sourceType, kindName(dstKind), kindName(srcKind))))
		return errs
	}
	// The kinds agree, which for a collection means only "both lists".
	if dv, ok := param.valueAt(lf.path); ok {
		if agree, want, got := celexpr.ElementsCompatible(srcType, dv); !agree {
			errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
				fmt.Sprintf("type mismatch for parameter %q of SourceDefinition %q: expected %s, got %s",
					lf.path, sourceType, want, got)))
		}
	}
	return errs
}

// parameterBlockSources memoises the reduction below, keyed on the template.
//
// Every admission re-parsed each definition a validated Application references,
// walked its declarations and re-formatted the result, to recover text fixed for
// the life of the definition. Measured on an ordinary component template, the
// whole of parameterBlockOnly is 104us, of which the reduction is 43us.
//
// Only the text is kept, never the compiled value. cue documents that "values
// created from the same Context are not safe for concurrent use", and admission
// requests are concurrent, so a shared cue.Value would be a data race rather
// than a saving. The compile therefore still happens per call - 56us of the
// 104us that cannot be recovered without a change in that guarantee.
//
// Keyed on the template text, so a definition that changes gets a new entry and
// there is no invalidation to get wrong.
var parameterBlockSources sync.Map // template -> parameterBlockExtract

// sourcesDisabledMessage says which of the two switches is off, because "not
// enabled" sends an author to the wrong one half the time.
func sourcesDisabledMessage() string {
	if !utilfeature.DefaultMutableFeatureGate.Enabled(features.EnableCelExpressions) {
		return "source expressions are not enabled on this cluster; " +
			"an operator enables them with the EnableCelExpressions feature gate"
	}
	return fmt.Sprintf("this Application has not opted in to source expressions; "+
		"set the %s annotation to \"true\", and check its properties for $(VAR) "+
		"environment-variable syntax, which must be written $$(VAR) once expressions are read",
		oam.AnnotationCelExpressions)
}
