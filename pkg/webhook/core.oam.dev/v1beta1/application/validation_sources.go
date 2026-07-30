package application

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/cuecontext"
	cueformat "cuelang.org/go/cue/format"
	cueparser "cuelang.org/go/cue/parser"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	upstreamcuex "github.com/kubevela/pkg/cue/cuex"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	velacue "github.com/oam-dev/kubevela/pkg/cue"
	velacuex "github.com/oam-dev/kubevela/pkg/cue/cuex"
	"github.com/oam-dev/kubevela/pkg/oam"
	oamutil "github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/pkg/webhook/core.oam.dev/v1beta1/sourcedefinition"
)

type fromSourceReference struct {
	SourceName       string
	Path             string
	FieldPath        *field.Path
	FromSourceObject bool
	SourceIndex      int
	// Surface is where the directive was found: a component, a trait, a policy,
	// a workflow step, or another source's properties (chaining).
	Surface string
}

// Surfaces a fromSource directive can appear on. Only components and traits are
// resolved today; see surfaceResolvesFromSource.
const (
	surfaceComponent    = "component"
	surfaceTrait        = "trait"
	surfacePolicy       = "policy"
	surfaceWorkflowStep = "workflow step"
	surfaceSource       = "source"
)

// surfaceResolvesFromSource reports whether fromSource is substituted on this
// surface at reconcile time. Resolution is wired into the component and trait
// render paths only; a directive anywhere else would pass admission and then be
// handed to the consumer as a literal {"fromSource": ...} map.
func surfaceResolvesFromSource(surface string) bool {
	switch surface {
	case surfaceComponent, surfaceTrait, surfaceSource:
		return true
	default:
		return false
	}
}

// withSurface stamps the surface onto each collected reference.
func withSurface(refs []fromSourceReference, surface string) []fromSourceReference {
	for i := range refs {
		refs[i].Surface = surface
	}
	return refs
}

type sourceSchemaValidator struct {
	schema cue.Value
}

// ValidateSources validates source bindings and fromSource references.
func (h *ValidatingHandler) ValidateSources(ctx context.Context, app *v1beta1.Application) field.ErrorList {
	var errs field.ErrorList

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

	var refs []fromSourceReference
	for i, comp := range app.Spec.Components {
		compRefs, refErrs := collectFromRawExtension(comp.Properties, field.NewPath("spec", "components").Index(i).Child("properties"), -1)
		errs = append(errs, refErrs...)
		refs = append(refs, withSurface(compRefs, surfaceComponent)...)
		for j, tr := range comp.Traits {
			trRefs, trErrs := collectFromRawExtension(tr.Properties, field.NewPath("spec", "components").Index(i).Child("traits").Index(j).Child("properties"), -1)
			errs = append(errs, trErrs...)
			refs = append(refs, withSurface(trRefs, surfaceTrait)...)
		}
	}
	for i, policy := range app.Spec.Policies {
		policyRefs, policyErrs := collectFromRawExtension(policy.Properties, field.NewPath("spec", "policies").Index(i).Child("properties"), -1)
		errs = append(errs, policyErrs...)
		refs = append(refs, withSurface(policyRefs, surfacePolicy)...)
	}
	if app.Spec.Workflow != nil {
		for i, step := range app.Spec.Workflow.Steps {
			stepRefs, stepErrs := collectFromRawExtension(step.Properties, field.NewPath("spec", "workflow", "steps").Index(i).Child("properties"), -1)
			errs = append(errs, stepErrs...)
			refs = append(refs, withSurface(stepRefs, surfaceWorkflowStep)...)
			for j, sub := range step.SubSteps {
				subRefs, subErrs := collectFromRawExtension(sub.Properties, field.NewPath("spec", "workflow", "steps").Index(i).Child("subSteps").Index(j).Child("properties"), -1)
				errs = append(errs, subErrs...)
				refs = append(refs, withSurface(subRefs, surfaceWorkflowStep)...)
			}
		}
	}
	for i, src := range app.Spec.Sources {
		srcRefs, srcErrs := collectFromRawExtension(src.Properties, field.NewPath("spec", "sources").Index(i).Child("properties"), i)
		errs = append(errs, srcErrs...)
		refs = append(refs, withSurface(srcRefs, surfaceSource)...)
	}

	schemaValidators := map[string]*sourceSchemaValidator{}
	consumableFromCache := map[string][]string{}
	for _, ref := range refs {
		sourceType, ok := sourceNameToType[ref.SourceName]
		if !ok {
			errs = append(errs, field.Invalid(ref.FieldPath, ref.SourceName, "source is not declared in spec.sources"))
			continue
		}
		if ref.SourceIndex >= 0 {
			depIdx, exists := sourceNameToIndex[ref.SourceName]
			if !exists {
				errs = append(errs, field.Invalid(ref.FieldPath, ref.SourceName, "source is not declared in spec.sources"))
				continue
			}
			if depIdx >= ref.SourceIndex {
				errs = append(errs, field.Invalid(ref.FieldPath, ref.SourceName,
					fmt.Sprintf("source at index %d can only depend on prior sources, but %q is at index %d", ref.SourceIndex, ref.SourceName, depIdx)))
				continue
			}
		}
		// fromSource is only substituted during component and trait rendering.
		// Elsewhere the consumer would receive the literal {"fromSource": ...}
		// map, so reject it rather than admitting a directive that silently
		// never resolves.
		if !surfaceResolvesFromSource(ref.Surface) {
			errs = append(errs, field.Invalid(ref.FieldPath, ref.Path,
				fmt.Sprintf("fromSource is not supported in %s properties; it is resolved during component and trait rendering only", ref.Surface)))
			continue
		}
		if sourceType == "" {
			continue
		}
		// A SourceDefinition may restrict where it can be consumed from.
		if ref.Surface == surfaceComponent || ref.Surface == surfaceTrait {
			surfaces, err := h.loadConsumableFrom(ctx, app.Namespace, sourceType, consumableFromCache)
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
		validator, exists := schemaValidators[sourceType]
		if !exists {
			var err error
			validator, err = h.loadSourceSchemaValidator(ctx, app.Namespace, sourceType)
			if err != nil {
				errs = append(errs, field.Invalid(ref.FieldPath, ref.Path, fmt.Sprintf("failed to load SourceDefinition %q schema: %v", sourceType, err)))
				continue
			}
			schemaValidators[sourceType] = validator
		}
		if validator == nil {
			continue
		}
		if !validator.HasPath(ref.Path) {
			errs = append(errs, field.Invalid(ref.FieldPath, ref.Path,
				fmt.Sprintf("path %q is not declared in schema of SourceDefinition %q", ref.Path, sourceType)))
			continue
		}
		// The "optional source field consumed without a default" check is
		// target-aware (KEP: a default is required only when the optional field
		// feeds a REQUIRED target parameter). It is enforced in the target-aware
		// passes below (validateSourceInputs for source-property targets,
		// validateFromSourceTargetTypes for component/trait targets), which know
		// the target parameter's optional/required marker.
	}

	// Input contract: validate each source's properties against that
	// SourceDefinition's parameter: block (unknown fields + type compatibility).
	errs = append(errs, h.validateSourceInputs(ctx, app, sourceNameToType, schemaValidators)...)

	// Target contract: each fromSource output field's type must be compatible
	// with the consuming component/trait parameter it is substituted into.
	errs = append(errs, h.validateFromSourceTargetTypes(ctx, app, sourceNameToType, schemaValidators)...)

	return errs
}

// validateFromSourceTargetTypes type-checks each fromSource reference in
// component and trait properties against the target parameter it feeds: the
// source's schema output field kind must be compatible with the component/trait
// parameter field kind at the same property path. Purely static (CUE AST); no
// rendering. Best-effort per target: if the target parameter type cannot be
// determined, the check is skipped for that target (fail open).
func (h *ValidatingHandler) validateFromSourceTargetTypes(ctx context.Context, app *v1beta1.Application, sourceNameToType map[string]string, schemaValidators map[string]*sourceSchemaValidator) field.ErrorList {
	var errs field.ErrorList
	targetParams := map[string]*cueStruct{} // key: "component/<type>" or "trait/<type>"

	load := func(kind, defType string) *cueStruct {
		key := kind + "/" + defType
		if pv, ok := targetParams[key]; ok {
			return pv
		}
		pv, _ := h.loadTargetParameter(ctx, app.Namespace, kind, defType)
		targetParams[key] = pv
		return pv
	}

	check := func(leaves []inputLeaf, param *cueStruct, targetDesc string) {
		if param == nil {
			return
		}
		for _, lf := range leaves {
			if lf.fromSrc == nil || lf.path == "" {
				continue
			}
			dstKind, declared := param.kindAt(lf.path)
			if !declared {
				continue // consuming template may accept it via open struct; don't over-report
			}
			refName, refPath, _, err := parseFromSourceSelector(lf.fromSrc)
			if err != nil {
				continue
			}
			refType, ok := sourceNameToType[refName]
			if !ok || refType == "" {
				continue
			}
			sv := schemaValidators[refType]
			if sv == nil {
				var loadErr error
				sv, loadErr = h.loadSourceSchemaValidator(ctx, app.Namespace, refType)
				if loadErr != nil || sv == nil {
					continue
				}
				schemaValidators[refType] = sv
			}
			srcKind, ok := sv.KindAt(refPath)
			if !ok {
				continue
			}
			if !kindsCompatible(srcKind, dstKind) {
				errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
					fmt.Sprintf("type mismatch: fromSource %q.%s is %s but %s expects %s",
						refName, refPath, kindName(srcKind), targetDesc, kindName(dstKind))))
			}
			// KEP: a default is required only when an optional source field
			// feeds a required target parameter.
			if !selectorHasDefault(lf.fromSrc) && sv.IsOptionalPath(refPath) {
				if required, _ := param.requiredAt(lf.path); required {
					errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
						fmt.Sprintf("optional source field %q.%s feeds required %s; a default must be supplied via the fromSource map form",
							refName, refPath, targetDesc)))
				}
			}
		}
	}

	for i, comp := range app.Spec.Components {
		if comp.Properties != nil && len(comp.Properties.Raw) > 0 {
			base := field.NewPath("spec", "components").Index(i).Child("properties")
			check(flattenLeafPaths(comp.Properties.Raw, base), load("component", comp.Type), fmt.Sprintf("component %q parameter", comp.Type))
		}
		for j, tr := range comp.Traits {
			if tr.Properties == nil || len(tr.Properties.Raw) == 0 {
				continue
			}
			base := field.NewPath("spec", "components").Index(i).Child("traits").Index(j).Child("properties")
			check(flattenLeafPaths(tr.Properties.Raw, base), load("trait", tr.Type), fmt.Sprintf("trait %q parameter", tr.Type))
		}
	}
	return errs
}

// validateSourceInputs checks that every source binding's properties conform to
// the referenced SourceDefinition's parameter: block: no undeclared fields, and
// each provided value's type is compatible with the declared parameter type.
// Values fed by fromSource take their type from the referenced source's schema:
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
			pv, err = h.loadSourceParameter(ctx, app.Namespace, src.Type)
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
			errs = append(errs, h.checkInputLeaf(lf, pv, src.Type, sourceNameToType, schemaValidators, ctx, app.Namespace)...)
		}
	}
	return errs
}

// inputLeaf is a single scalar or fromSource value within a source's
// properties, addressed by its dotted path relative to the parameter block.
type inputLeaf struct {
	path      string     // dotted path into the parameter block, e.g. "region"
	fieldPath *field.Path // full field path for error reporting
	literal   interface{} // the scalar value, when not a fromSource
	fromSrc   interface{} // the fromSource selector, when present
}

// flattenLeafPaths walks a properties JSON blob and returns one inputLeaf per
// scalar or fromSource node, keyed by dotted path. A fromSource node is treated
// as a leaf (its name/path/default are not recursed into). Array elements are
// addressed by index. Returns nothing on unparseable input (the fromSource
// collection pass already reports JSON errors).
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
			if sel, ok := v["fromSource"]; ok {
				out = append(out, inputLeaf{path: dotted, fieldPath: fp.Child("fromSource"), fromSrc: sel})
				return
			}
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
// declared parameter type. fromSource-fed leaves take their type from the
// referenced source's schema output field.
func (h *ValidatingHandler) checkInputLeaf(lf inputLeaf, param *cueStruct, sourceType string, sourceNameToType map[string]string, schemaValidators map[string]*sourceSchemaValidator, ctx context.Context, appNamespace string) field.ErrorList {
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
	if lf.fromSrc != nil {
		refName, refPath, _, err := parseFromSourceSelector(lf.fromSrc)
		if err != nil {
			return errs // the collection pass already reported this
		}
		refType, ok := sourceNameToType[refName]
		if !ok || refType == "" {
			return errs // unknown source already reported by the ref pass
		}
		sv := schemaValidators[refType]
		if sv == nil {
			var loadErr error
			sv, loadErr = h.loadSourceSchemaValidator(ctx, appNamespace, refType)
			if loadErr != nil || sv == nil {
				return errs
			}
			schemaValidators[refType] = sv
		}
		k, ok := sv.KindAt(refPath)
		if !ok {
			return errs // path-not-in-schema already reported by the ref pass
		}
		srcKind = k
		// KEP: a default is required only when an optional source field feeds a
		// required target. Here the target is this SourceDefinition's parameter.
		if hasDefault := selectorHasDefault(lf.fromSrc); !hasDefault && sv.IsOptionalPath(refPath) {
			if required, _ := param.requiredAt(lf.path); required {
				errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
					fmt.Sprintf("optional source field %q.%s feeds required parameter %q of SourceDefinition %q; a default must be supplied via the fromSource map form",
						refName, refPath, lf.path, sourceType)))
			}
		}
	} else {
		srcKind = jsonKind(lf.literal)
	}
	if !kindsCompatible(srcKind, dstKind) {
		errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
			fmt.Sprintf("type mismatch for parameter %q of SourceDefinition %q: expected %s, got %s",
				lf.path, sourceType, kindName(dstKind), kindName(srcKind))))
	}
	return errs
}

// selectorHasDefault reports whether a fromSource selector (map form) carries a
// default: key.
func selectorHasDefault(selector interface{}) bool {
	m, ok := selector.(map[string]interface{})
	if !ok {
		return false
	}
	_, ok = m["default"]
	return ok
}

func collectFromRawExtension(raw *runtime.RawExtension, basePath *field.Path, sourceIndex int) ([]fromSourceReference, field.ErrorList) {
	if raw == nil || len(raw.Raw) == 0 {
		return nil, nil
	}
	var decoded interface{}
	if err := json.Unmarshal(raw.Raw, &decoded); err != nil {
		return nil, field.ErrorList{field.Invalid(basePath, string(raw.Raw), fmt.Sprintf("invalid properties JSON: %v", err))}
	}
	refs, errs := collectFromNode(decoded, basePath, sourceIndex)
	return refs, errs
}

func collectFromNode(node interface{}, path *field.Path, sourceIndex int) ([]fromSourceReference, field.ErrorList) {
	var refs []fromSourceReference
	var errs field.ErrorList
	switch v := node.(type) {
	case map[string]interface{}:
		if selector, ok := v["fromSource"]; ok {
			name, sourcePath, _, err := parseFromSourceSelector(selector)
			if err != nil {
				errs = append(errs, field.Invalid(path.Child("fromSource"), selector, err.Error()))
				return refs, errs
			}
			refs = append(refs, fromSourceReference{
				SourceName:  name,
				Path:        sourcePath,
				FieldPath:   path.Child("fromSource"),
				SourceIndex: sourceIndex,
			})
			return refs, errs
		}
		for key, child := range v {
			childRefs, childErrs := collectFromNode(child, path.Child(key), sourceIndex)
			refs = append(refs, childRefs...)
			errs = append(errs, childErrs...)
		}
	case []interface{}:
		for i, child := range v {
			childRefs, childErrs := collectFromNode(child, path.Index(i), sourceIndex)
			refs = append(refs, childRefs...)
			errs = append(errs, childErrs...)
		}
	}
	return refs, errs
}

func parseFromSourceSelector(selector interface{}) (name string, path string, hasDefault bool, err error) {
	switch v := selector.(type) {
	case string:
		parts := strings.SplitN(v, ".", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", false, fmt.Errorf("invalid fromSource reference %q", v)
		}
		return parts[0], parts[1], false, nil
	case map[string]interface{}:
		name, _ := v["name"].(string)
		path, _ := v["path"].(string)
		if name == "" || path == "" {
			return "", "", false, fmt.Errorf("fromSource requires both name and path")
		}
		_, hasDefault := v["default"]
		return name, path, hasDefault, nil
	default:
		return "", "", false, fmt.Errorf("invalid fromSource selector type %T", selector)
	}
}

// loadConsumableFrom returns the surfaces a SourceDefinition may be consumed
// from, memoised per source type. Nil means unrestricted.
func (h *ValidatingHandler) loadConsumableFrom(ctx context.Context, appNamespace, sourceType string, cache map[string][]string) ([]string, error) {
	if surfaces, ok := cache[sourceType]; ok {
		return surfaces, nil
	}
	def, err := h.getSourceDefinition(ctx, appNamespace, sourceType)
	if err != nil {
		return nil, err
	}
	var surfaces []string
	if def.Spec.Schematic != nil && def.Spec.Schematic.CUE != nil {
		surfaces, err = sourcedefinition.ParseConsumableFrom(def.Spec.Schematic.CUE.Template)
		if err != nil {
			return nil, err
		}
	}
	cache[sourceType] = surfaces
	return surfaces, nil
}

func (h *ValidatingHandler) loadSourceSchemaValidator(ctx context.Context, appNamespace, sourceType string) (*sourceSchemaValidator, error) {
	def, err := h.getSourceDefinition(ctx, appNamespace, sourceType)
	if err != nil {
		return nil, err
	}
	if def.Spec.Schematic == nil || def.Spec.Schematic.CUE == nil {
		return nil, nil
	}
	schemaExpr, err := extractSourceSchemaExprForAdmission(def.Spec.Schematic.CUE.Template)
	if err != nil {
		return nil, err
	}
	if schemaExpr == "" {
		return nil, nil
	}
	v := cuecontext.New().CompileString("schema: " + schemaExpr)
	if v.Err() != nil {
		return nil, v.Err()
	}
	schema := v.LookupPath(cue.ParsePath("schema"))
	if !schema.Exists() {
		return nil, nil
	}
	return &sourceSchemaValidator{schema: schema}, nil
}

// cueStruct wraps a struct cue.Value with dotted-path lookup helpers. It backs
// both the source schema (output contract) and the source/target parameter
// (input contract) validators; the path/type helpers are identical for both.
type cueStruct struct {
	root cue.Value
}

// lookup walks a dotted path through the struct, resolving optional fields the
// same way sourceSchemaValidator does.
func (c *cueStruct) lookup(path string) (cue.Value, bool) {
	cur := c.root
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return cur, false
		}
		if idx, err := strconv.Atoi(seg); err == nil {
			cur = cur.LookupPath(cue.MakePath(cue.Index(idx)))
			if !cur.Exists() {
				return cur, false
			}
			continue
		}
		next := cur.LookupPath(cue.MakePath(cue.Str(seg)))
		if !next.Exists() {
			if opt, ok := lookupOptionalField(cur, seg); ok {
				cur = opt
				continue
			}
			return next, false
		}
		cur = next
	}
	return cur, true
}

// kindAt returns the declared CUE kind at path (e.g. StringKind, IntKind,
// StructKind). Returns (BottomKind, false) if the path does not resolve.
func (c *cueStruct) kindAt(path string) (cue.Kind, bool) {
	v, ok := c.lookup(path)
	if !ok || !v.Exists() {
		return cue.BottomKind, false
	}
	return v.IncompleteKind(), true
}

// requiredAt reports whether path names a field that the struct declares AND
// requires (i.e. present and not optional). Returns (required, declared).
// A field with a default is not required (it has a fallback value).
func (c *cueStruct) requiredAt(path string) (required bool, declared bool) {
	segs := strings.Split(path, ".")
	if len(segs) == 0 || segs[len(segs)-1] == "" {
		return false, false
	}
	leaf := segs[len(segs)-1]
	if _, err := strconv.Atoi(leaf); err == nil {
		return false, false // array index: not a named required field
	}
	parent := c.root
	if len(segs) > 1 {
		p, ok := c.lookup(strings.Join(segs[:len(segs)-1], "."))
		if !ok {
			return false, false
		}
		parent = p
	}
	iter, err := parent.Fields(cue.Optional(true), cue.Definitions(false))
	if err != nil {
		return false, false
	}
	for iter.Next() {
		sel := iter.Selector()
		if !sel.IsString() || sel.Unquoted() != leaf {
			continue
		}
		if iter.IsOptional() {
			return false, true
		}
		if _, hasDefault := iter.Value().Default(); hasDefault {
			return false, true // defaulted -> not required
		}
		return true, true
	}
	return false, false
}

// kindName renders a CUE kind for user-facing error messages.
func kindName(k cue.Kind) string {
	switch k {
	case cue.StringKind:
		return "string"
	case cue.IntKind:
		return "int"
	case cue.NumberKind, cue.FloatKind:
		return "number"
	case cue.BoolKind:
		return "bool"
	case cue.StructKind:
		return "object"
	case cue.ListKind:
		return "list"
	case cue.NullKind:
		return "null"
	}
	return k.String()
}

// kindsCompatible reports whether a value of kind src can satisfy a target of
// kind dst. Compatibility is by kind intersection, which is permissive enough
// to avoid false positives from value-level constraints (enums, bounds) while
// still catching genuine mismatches such as string into int. int is accepted
// where number is expected.
func kindsCompatible(src, dst cue.Kind) bool {
	if src == cue.BottomKind || dst == cue.BottomKind {
		return true // unknown on either side: do not block
	}
	if src&dst != 0 {
		return true
	}
	// int is a subset of number/float.
	if src == cue.IntKind && dst&(cue.NumberKind|cue.FloatKind) != 0 {
		return true
	}
	if dst == cue.IntKind && src&(cue.NumberKind|cue.FloatKind) != 0 {
		return true
	}
	return false
}

// loadSourceParameter returns a validator over the SourceDefinition's top-level
// parameter: block, or nil if the definition declares no parameter block.
func (h *ValidatingHandler) loadSourceParameter(ctx context.Context, appNamespace, sourceType string) (*cueStruct, error) {
	def, err := h.getSourceDefinition(ctx, appNamespace, sourceType)
	if err != nil {
		return nil, err
	}
	if def.Spec.Schematic == nil || def.Spec.Schematic.CUE == nil {
		return nil, nil
	}
	paramExpr, err := extractTopLevelBlock(def.Spec.Schematic.CUE.Template, "parameter")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(paramExpr) == "" {
		return nil, nil
	}
	v := cuecontext.New().CompileString("parameter: " + paramExpr)
	if v.Err() != nil {
		return nil, v.Err()
	}
	param := v.LookupPath(cue.ParsePath("parameter"))
	if !param.Exists() {
		return nil, nil
	}
	return &cueStruct{root: param}, nil
}

// loadTargetParameter returns a validator over the parameter: block of a
// ComponentDefinition (kind "component") or TraitDefinition (kind "trait"),
// used to type-check fromSource-fed values against the consuming parameter.
// This is best-effort: any failure (definition not found, template does not
// compile statically, no parameter block) yields (nil, nil) so validation
// fails open rather than blocking a legitimate apply.
func (h *ValidatingHandler) loadTargetParameter(ctx context.Context, appNamespace, kind, defName string) (*cueStruct, error) {
	tmpl, ok := h.getDefinitionTemplate(ctx, appNamespace, kind, defName)
	if !ok || strings.TrimSpace(tmpl) == "" {
		return nil, nil
	}
	// Compile statically with provider imports available but provider functions
	// disabled (we only need declared types, no rendering / I/O).
	val, err := velacuex.WorkloadCompiler.Get().CompileStringWithOptions(
		ctx, tmpl+velacue.BaseTemplate, upstreamcuex.DisableResolveProviderFunctions{})
	if err != nil || val.Err() != nil {
		klog.V(4).Infof("skip target parameter type check for %s %q: template did not compile statically", kind, defName)
		return nil, nil
	}
	param := val.LookupPath(cue.ParsePath("parameter"))
	if !param.Exists() {
		return nil, nil
	}
	return &cueStruct{root: param}, nil
}

// getDefinitionTemplate fetches a Component/Trait definition (app namespace with
// system-namespace fallback) and returns its CUE template string.
func (h *ValidatingHandler) getDefinitionTemplate(ctx context.Context, appNamespace, kind, defName string) (string, bool) {
	lookupCtx := oamutil.SetNamespaceInCtx(ctx, appNamespace)
	switch kind {
	case "component":
		def := &v1beta1.ComponentDefinition{}
		if err := oamutil.GetDefinition(lookupCtx, h.Client, def, defName); err != nil {
			return "", false
		}
		if def.Spec.Schematic == nil || def.Spec.Schematic.CUE == nil {
			return "", false
		}
		return def.Spec.Schematic.CUE.Template, true
	case "trait":
		def := &v1beta1.TraitDefinition{}
		if err := oamutil.GetDefinition(lookupCtx, h.Client, def, defName); err != nil {
			return "", false
		}
		if def.Spec.Schematic == nil || def.Spec.Schematic.CUE == nil {
			return "", false
		}
		return def.Spec.Schematic.CUE.Template, true
	}
	return "", false
}

func (h *ValidatingHandler) getSourceDefinition(ctx context.Context, appNamespace, sourceType string) (*v1beta1.SourceDefinition, error) {
	def := &v1beta1.SourceDefinition{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: appNamespace, Name: sourceType}, def); err == nil {
		return def, nil
	} else if !errors.IsNotFound(err) {
		return nil, err
	}
	if appNamespace != oam.SystemDefinitionNamespace {
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: oam.SystemDefinitionNamespace, Name: sourceType}, def); err == nil {
			return def, nil
		} else if !errors.IsNotFound(err) {
			return nil, err
		}
	}
	return nil, errors.NewNotFound(schema.GroupVersionResource{Group: v1beta1.Group, Version: v1beta1.Version, Resource: "sourcedefinitions"}.GroupResource(), sourceType)
}

func (v *sourceSchemaValidator) HasPath(path string) bool {
	cur, ok := v.lookup(path)
	return ok && cur.Exists()
}

// KindAt returns the declared CUE kind of the schema output field at path.
func (v *sourceSchemaValidator) KindAt(path string) (cue.Kind, bool) {
	cur, ok := v.lookup(path)
	if !ok || !cur.Exists() {
		return cue.BottomKind, false
	}
	return cur.IncompleteKind(), true
}

// IsOptionalPath reports whether the final segment of path is declared optional
// (e.g. `field?:`) in the schema. Returns false if the path does not resolve or
// the final segment is an array index. LookupPath strips the optional marker
// from the returned value, so optionality is detected by iterating the parent
// struct's fields and matching the leaf label.
func (v *sourceSchemaValidator) IsOptionalPath(path string) bool {
	segs := strings.Split(path, ".")
	if len(segs) == 0 {
		return false
	}
	parentPath := strings.Join(segs[:len(segs)-1], ".")
	leaf := segs[len(segs)-1]
	if leaf == "" {
		return false
	}
	if _, err := strconv.Atoi(leaf); err == nil {
		// array element: optionality is not a meaningful concept here
		return false
	}
	parent := v.schema
	if parentPath != "" {
		p, ok := v.lookup(parentPath)
		if !ok {
			return false
		}
		parent = p
	}
	iter, err := parent.Fields(cue.Optional(true), cue.Definitions(false))
	if err != nil {
		return false
	}
	for iter.Next() {
		sel := iter.Selector()
		if sel.IsString() && sel.Unquoted() == leaf {
			return iter.IsOptional()
		}
	}
	return false
}

// lookup walks the dotted path through the schema value and returns the reached
// value plus whether every segment resolved. Optional fields (field?:) are not
// returned by LookupPath, so a failed struct lookup falls back to iterating the
// parent's fields (including optionals) to locate the segment.
func (v *sourceSchemaValidator) lookup(path string) (cue.Value, bool) {
	cur := v.schema
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return cur, false
		}
		if idx, err := strconv.Atoi(seg); err == nil {
			cur = cur.LookupPath(cue.MakePath(cue.Index(idx)))
			if !cur.Exists() {
				return cur, false
			}
			continue
		}
		next := cur.LookupPath(cue.MakePath(cue.Str(seg)))
		if !next.Exists() {
			if opt, ok := lookupOptionalField(cur, seg); ok {
				cur = opt
				continue
			}
			return next, false
		}
		cur = next
	}
	return cur, true
}

// lookupOptionalField finds a field by label in parent (including optional
// fields) and returns its value.
func lookupOptionalField(parent cue.Value, label string) (cue.Value, bool) {
	iter, err := parent.Fields(cue.Optional(true), cue.Definitions(false))
	if err != nil {
		return cue.Value{}, false
	}
	for iter.Next() {
		sel := iter.Selector()
		if sel.IsString() && sel.Unquoted() == label {
			return iter.Value(), true
		}
	}
	return cue.Value{}, false
}

func extractSourceSchemaExprForAdmission(template string) (string, error) {
	return extractTopLevelBlock(template, "schema")
}

// extractTopLevelBlock returns the CUE source of the top-level field named
// blockName (e.g. "schema" or "parameter") from a SourceDefinition template, or
// "" if absent. Static parse only; no evaluation.
func extractTopLevelBlock(template, blockName string) (string, error) {
	file, err := cueparser.ParseFile("-", template, cueparser.ParseComments)
	if err != nil {
		return "", err
	}
	for _, decl := range file.Decls {
		field, ok := decl.(*ast.Field)
		if !ok {
			continue
		}
		name, _, err := ast.LabelName(field.Label)
		if err != nil || name != blockName {
			continue
		}
		bt, err := cueformat.Node(field.Value)
		if err != nil {
			return "", err
		}
		return string(bt), nil
	}
	return "", nil
}
