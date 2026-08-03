package application

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	cueast "cuelang.org/go/cue/ast"
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
	"github.com/oam-dev/kubevela/pkg/appfile"
	velacue "github.com/oam-dev/kubevela/pkg/cue"
	velacuex "github.com/oam-dev/kubevela/pkg/cue/cuex"
	veladefinition "github.com/oam-dev/kubevela/pkg/cue/definition"
	"github.com/oam-dev/kubevela/pkg/definition/cachekey"
	"github.com/oam-dev/kubevela/pkg/definition/sourceexpr"
	"github.com/oam-dev/kubevela/pkg/oam"
	oamutil "github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/pkg/webhook/core.oam.dev/v1beta1/sourcedefinition"
)

type sourceReference struct {
	SourceName  string
	Path        string
	FieldPath   *field.Path
	SourceIndex int
	// OpaquePath marks a path the schema validator cannot follow by dotted
	// lookup - one carrying a list index, or a key that itself contains a dot.
	OpaquePath bool
	// Surface is where the read was found: a component, a trait, a policy, a
	// workflow step, or another source's properties (chaining).
	Surface string
}

// withSurface stamps the surface onto each collected reference.
func withSurface(refs []sourceReference, surface string) []sourceReference {
	for i := range refs {
		refs[i].Surface = surface
	}
	return refs
}

type sourceSchemaValidator struct {
	schema cue.Value
	// schemaExpr is the schema block as source text, retained because typing an
	// expression needs a schema it can build sentinels from, and re-extracting it
	// would mean a second definition lookup.
	schemaExpr string
}

// ValidateSources validates source bindings and the source reads expressions make.
func (h *ValidatingHandler) ValidateSources(ctx context.Context, app *v1beta1.Application) field.ErrorList {
	var errs field.ErrorList

	// Expression syntax and sandbox first: it needs no definition lookups, so a
	// typo is reported even when the rest of validation cannot run.
	errs = append(errs, validateExpressions(app)...)

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
		refs = append(refs, withSurface(compRefs, veladefinition.SurfaceComponent)...)
		for j, tr := range comp.Traits {
			trRefs, trErrs := collectSourceRefs(tr.Properties, field.NewPath("spec", "components").Index(i).Child("traits").Index(j).Child("properties"), -1)
			errs = append(errs, trErrs...)
			refs = append(refs, withSurface(trRefs, veladefinition.SurfaceTrait)...)
		}
	}
	for i, policy := range app.Spec.Policies {
		policyRefs, policyErrs := collectSourceRefs(policy.Properties, field.NewPath("spec", "policies").Index(i).Child("properties"), -1)
		errs = append(errs, policyErrs...)
		surface := veladefinition.SurfacePolicy
		if !appfile.IsBuiltinPolicyType(policy.Type) {
			surface = veladefinition.SurfacePolicyRendered
		}
		refs = append(refs, withSurface(policyRefs, surface)...)
	}
	if app.Spec.Workflow != nil {
		for i, step := range app.Spec.Workflow.Steps {
			stepRefs, stepErrs := collectSourceRefs(step.Properties, field.NewPath("spec", "workflow", "steps").Index(i).Child("properties"), -1)
			errs = append(errs, stepErrs...)
			refs = append(refs, withSurface(stepRefs, veladefinition.SurfaceWorkflowStep)...)
			for j, sub := range step.SubSteps {
				subRefs, subErrs := collectSourceRefs(sub.Properties, field.NewPath("spec", "workflow", "steps").Index(i).Child("subSteps").Index(j).Child("properties"), -1)
				errs = append(errs, subErrs...)
				refs = append(refs, withSurface(subRefs, veladefinition.SurfaceWorkflowStep)...)
			}
		}
	}
	for i, src := range app.Spec.Sources {
		srcRefs, srcErrs := collectSourceRefs(src.Properties, field.NewPath("spec", "sources").Index(i).Child("properties"), i)
		errs = append(errs, srcErrs...)
		refs = append(refs, withSurface(srcRefs, veladefinition.SurfaceSource)...)
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
		if ref.Surface == veladefinition.SurfaceComponent || ref.Surface == veladefinition.SurfaceTrait {
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

		// A source resolves in its call site's context, so it can only be
		// consumed where every field its template reads exists. A chained source
		// resolves in whichever render triggered the outer binding, so the
		// surfaces it must satisfy are its consumers', not its own - which is
		// what effectiveSurfaces works out.
		required, rerr := h.requiredContext(ctx, app.Namespace, sourceType, requiredContextCache)
		if rerr == nil && len(required) > 0 {
			for _, surface := range effective[ref.SourceName] {
				if cerr := cachekey.CheckSurface(required, surface); cerr != nil {
					fault(ref, ref.Path, fmt.Sprintf("SourceDefinition %q %v", sourceType, cerr))
					break
				}
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
	if raw, isString := lf.literal.(string); isString && hasSourceExpression(raw) {
		// A source's own properties may be fed by an expression - that is how
		// chaining is written without the directive. Typing it as the string it
		// literally is would reject every non-string target.
		k, terr := h.expressionKind(ctx, appNamespace, raw, sourceNameToType, schemaValidators)
		if terr != nil {
			errs = append(errs, field.Invalid(lf.fieldPath, raw, terr.Error()))
			return errs
		}
		srcKind = k

		// The same optional-feeds-required rule the directive follows.
		if undefended := h.undefendedExpressionReads(ctx, appNamespace, raw, sourceNameToType, schemaValidators); len(undefended) > 0 {
			if required, _ := param.requiredAt(lf.path); required {
				errs = append(errs, field.Invalid(lf.fieldPath, lf.path,
					fmt.Sprintf("%s may be absent and feeds required parameter %q of SourceDefinition %q; supply a default with *%s | <fallback>",
						undefended[0], lf.path, sourceType, undefended[0])))
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

// collectSourceRefs returns every `source` read an expression makes within a
// properties blob, as the reference records the validation loop consumes.
//
// The loop is mechanism-agnostic: declared-ness, chaining order, surface and
// consumableFrom are properties of *reading a source*, not of how the read was
// spelled. Only this collector knew about the directive form, which is what let
// it be removed without losing a single one of those checks.
func collectSourceRefs(raw *runtime.RawExtension, basePath *field.Path, sourceIndex int) ([]sourceReference, field.ErrorList) {
	if raw == nil || len(raw.Raw) == 0 {
		return nil, nil
	}
	var decoded interface{}
	if err := json.Unmarshal(raw.Raw, &decoded); err != nil {
		return nil, field.ErrorList{field.Invalid(basePath, string(raw.Raw),
			fmt.Sprintf("invalid properties: %v", err))}
	}

	var refs []sourceReference
	for _, lf := range flattenLeafPaths(raw.Raw, basePath) {
		text, ok := lf.literal.(string)
		if !ok {
			continue
		}
		parsed, err := sourceexpr.Parse(text)
		if err != nil || !parsed.HasExpr() {
			continue
		}
		for _, fragment := range parsed.Fragments {
			if !fragment.IsExpr() {
				continue
			}
			reads, rerr := sourceexpr.References(fragment.Expr)
			if rerr != nil {
				// Syntax errors are reported by validateExpressions with a
				// better message; do not report them twice.
				continue
			}
			for _, read := range reads {
				if !read.IsSource() || len(read.Path) < 2 {
					continue
				}
				refs = append(refs, sourceReference{
					SourceName: read.Path[0],
					Path:       strings.Join(read.Path[1:], "."),
					// Whether the dotted form round-trips has to be decided here,
					// while the segments are still separate. `labels["a.b/c"]`
					// joins to `labels.a.b/c`, which no longer says where the key
					// began - and a list index joins to a segment the schema has
					// no field for at all.
					OpaquePath:  pathIsOpaque(read.Path[1:]),
					FieldPath:   lf.fieldPath,
					SourceIndex: sourceIndex,
				})
			}
		}
	}
	return refs, nil
}

// pathIsOpaque reports a path the schema validator's dotted lookup cannot
// follow: one carrying a list index, or a key that itself contains a dot.
//
// Both are ordinary reads - `outputs[0].kind`, `labels["platform.io/team"]` -
// and TypeOf checks them properly, against the element type and the map's
// pattern constraint. The coarser HasPath check is skipped for them rather than
// left to reject a valid read, which is what it did to every label key with a
// domain-prefixed name.
func pathIsOpaque(segments []string) bool {
	for _, segment := range segments {
		if strings.Contains(segment, ".") {
			return true
		}
		if segment == "" {
			continue
		}
		digits := true
		for _, r := range segment {
			if r < '0' || r > '9' {
				digits = false
				break
			}
		}
		if digits {
			return true
		}
	}
	return false
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
	return &sourceSchemaValidator{schema: schema, schemaExpr: schemaExpr}, nil
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
			next := cur.LookupPath(cue.MakePath(cue.Index(idx)))
			if !next.Exists() {
				// An open list - [...string] - has no concrete element at any
				// index, only an element type. Without this a source property
				// like items: ["a","b"] is flattened to items.0 / items.1 and
				// then reported as undeclared, which is how a perfectly valid
				// list-valued property was being rejected at admission.
				next = cur.LookupPath(cue.MakePath(cue.AnyIndex))
			}
			if !next.Exists() {
				return next, false
			}
			cur = next
			continue
		}
		next := cur.LookupPath(cue.MakePath(cue.Str(seg)))
		if !next.Exists() {
			if opt, ok := lookupOptionalField(cur, seg); ok {
				cur = opt
				continue
			}
			// An open map - headers?: [string]: string - declares no concrete
			// field at any key, only a value type. Without this, passing any
			// header at all was reported as "not declared in the parameter
			// schema", which is the open-list bug wearing a different hat.
			if pattern := cur.LookupPath(cue.MakePath(cue.AnyString)); pattern.Exists() {
				cur = pattern
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
// used to type-check expression-fed values against the consuming parameter.
// This is best-effort: any failure (definition not found, template does not
// compile statically, no parameter block) yields (nil, nil) so validation
// fails open rather than blocking a legitimate apply.
func (h *ValidatingHandler) loadTargetParameter(ctx context.Context, appNamespace, kind, defName string) (*cueStruct, error) {
	tmpl, ok := h.getDefinitionTemplate(ctx, appNamespace, kind, defName)
	if !ok || strings.TrimSpace(tmpl) == "" {
		return nil, nil
	}
	// Only the `parameter:` block is wanted, so only that is compiled.
	//
	// Compiling the whole template needs every package it imports to be
	// registered with the compiler in hand, and WorkloadCompiler carries the
	// workload providers - not vela/multicluster or vela/builtin. So every
	// workflow-step definition failed to compile and the check silently passed,
	// which is why a type mismatch in a step surfaced as a Go unmarshal error
	// instead. The same applied to any component definition importing a package
	// this compiler does not hold.
	if param, ok := parameterBlockOnly(ctx, tmpl); ok {
		return param, nil
	}

	// Fall back to compiling the whole template: a `parameter` that references
	// something else in the file needs the file.
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

// parameterBlockOnly extracts a template's `parameter:` declaration and compiles
// it on its own, without the imports or the body around it.
//
// A definition's parameter block is a plain type declaration - it is the
// contract an author writes against - so it almost never needs the rest of the
// file. Compiling it alone makes the type check independent of which providers
// the compiler happens to hold, which is what stopped it running for workflow
// steps at all.
func parameterBlockOnly(ctx context.Context, tmpl string) (*cueStruct, bool) {
	file, err := cueparser.ParseFile("-", tmpl, cueparser.ParseComments)
	if err != nil || file == nil {
		return nil, false
	}

	// The parameter field, plus any top-level definitions it might reference.
	var keep []cueast.Decl
	found := false
	for _, decl := range file.Decls {
		field, ok := decl.(*cueast.Field)
		if !ok {
			continue
		}
		name, _, lerr := cueast.LabelName(field.Label)
		if lerr != nil {
			continue
		}
		switch {
		case name == "parameter":
			keep = append(keep, decl)
			found = true
		case strings.HasPrefix(name, "#"):
			keep = append(keep, decl)
		}
	}
	if !found {
		return nil, false
	}

	src, ferr := cueformat.Node(&cueast.File{Decls: keep})
	if ferr != nil {
		return nil, false
	}
	val := cuecontext.New().CompileBytes(src)
	if val.Err() != nil {
		return nil, false
	}
	param := val.LookupPath(cue.ParsePath("parameter"))
	if !param.Exists() {
		return nil, false
	}
	return &cueStruct{root: param}, true
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
	case "workflowstep":
		def := &v1beta1.WorkflowStepDefinition{}
		if err := oamutil.GetDefinition(lookupCtx, h.Client, def, defName); err != nil {
			return "", false
		}
		if def.Spec.Schematic == nil || def.Spec.Schematic.CUE == nil {
			return "", false
		}
		return def.Spec.Schematic.CUE.Template, true
	case "policy":
		def := &v1beta1.PolicyDefinition{}
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
		// `content: _` or `properties: _` declares a field without declaring its
		// shape, so nothing below it is knowable here. TypeOf is what judges
		// those reads - it demands `& <type>` at the point of use - and this
		// check has nothing to add beyond rejecting a read the schema
		// deliberately left open.
		if cur.IncompleteKind() == cue.TopKind {
			return cur, true
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
			// An open map declares a pattern, never a key: `traits: [string]: {...}`
			// has no field called `scaler`, but reading one is exactly what the
			// map is for. Fall through to the pattern's type, which is what the
			// key will hold. Without this every key read out of a declared map -
			// traits, labels, a Config's outputs - was rejected as undeclared.
			if pattern := cur.LookupPath(cue.MakePath(cue.AnyString)); pattern.Exists() {
				cur = pattern
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

// effectiveSurfaces maps each source binding to the surfaces it really resolves
// on, following chains.
//
// A binding consumed by a component resolves in a component's context. A binding
// consumed only by another source resolves wherever *that* source is consumed -
// so the surfaces propagate backwards along the chain, and a source used only for
// chaining inherits every surface its consumers are used from.
//
// Chains are acyclic by construction: admission already refuses a source that
// depends on a later one, so a fixpoint converges.
func effectiveSurfaces(refs []sourceReference, bindingAt map[int]string) map[string][]string {
	direct := map[string]map[string]bool{}
	// consumers[a] are the bindings whose own properties read a.
	consumers := map[string][]string{}

	for _, ref := range refs {
		if ref.SourceIndex >= 0 {
			// A read inside spec.sources[i], so the reader is that binding.
			if reader, ok := bindingAt[ref.SourceIndex]; ok {
				consumers[ref.SourceName] = append(consumers[ref.SourceName], reader)
			}
			continue
		}
		if direct[ref.SourceName] == nil {
			direct[ref.SourceName] = map[string]bool{}
		}
		direct[ref.SourceName][ref.Surface] = true
	}

	out := map[string][]string{}
	for name, set := range direct {
		for surface := range set {
			out[name] = append(out[name], surface)
		}
	}
	// Propagate until stable. The graph is small and acyclic; a bounded loop
	// keeps a malformed spec from spinning.
	for i := 0; i < len(refs)+1; i++ {
		changed := false
		for name, readers := range consumers {
			have := map[string]bool{}
			for _, s := range out[name] {
				have[s] = true
			}
			for _, reader := range readers {
				for _, s := range out[reader] {
					if !have[s] {
						have[s] = true
						out[name] = append(out[name], s)
						changed = true
					}
				}
			}
		}
		if !changed {
			break
		}
	}
	for name := range out {
		sort.Strings(out[name])
	}
	return out
}

// requiredContext returns the context fields a SourceDefinition's template reads,
// memoised per source type.
func (h *ValidatingHandler) requiredContext(ctx context.Context, appNamespace, sourceType string,
	cache map[string][]string) ([]string, error) {
	if fields, ok := cache[sourceType]; ok {
		return fields, nil
	}
	def, err := h.getSourceDefinition(ctx, appNamespace, sourceType)
	if err != nil {
		return nil, err
	}
	if def.Spec.Schematic == nil || def.Spec.Schematic.CUE == nil {
		cache[sourceType] = nil
		return nil, nil
	}
	fields, err := cachekey.RequiredContext(def.Spec.Schematic.CUE.Template)
	if err != nil {
		return nil, err
	}
	cache[sourceType] = fields
	return fields, nil
}

// validateSourceContextReads checks the context an Application reads inside
// spec.sources[].properties against the surfaces that consume each binding.
//
// This is the other half of surface compatibility, and the half that is reachable
// today. A SourceDefinition's own template may only read universally-available
// context, so it can be consumed anywhere - but the *Application* can feed a
// source from context, which is how a per-component source is written:
//
//	sources:
//	  - name: own
//	    type: percomp
//	    properties: {component: '$(context.componentName)'}
//
// That binding now only works where componentName exists. Consumed from a
// workflow step, the read has nothing to resolve against - and the failure was
// silent: the step's expressions were left unsubstituted and the literal
// "$(source.own.label)" was written into the rendered resource.
func validateSourceContextReads(app *v1beta1.Application, effective map[string][]string) field.ErrorList {
	var errs field.ErrorList
	for i, src := range app.Spec.Sources {
		if src.Properties == nil || len(src.Properties.Raw) == 0 || src.Name == "" {
			continue
		}
		base := field.NewPath("spec", "sources").Index(i).Child("properties")
		for _, lf := range flattenLeafPaths(src.Properties.Raw, base) {
			text, ok := lf.literal.(string)
			if !ok {
				continue
			}
			parsed, perr := sourceexpr.Parse(text)
			if perr != nil || !parsed.HasExpr() {
				continue
			}
			for _, fragment := range parsed.Fragments {
				if !fragment.IsExpr() {
					continue
				}
				reads, rerr := sourceexpr.References(fragment.Expr)
				if rerr != nil {
					continue // reported by validateExpressions
				}
				for _, read := range reads {
					if read.IsSource() || len(read.Path) == 0 {
						continue
					}
					for _, surface := range effective[src.Name] {
						if sourceexpr.ContextFor(surface).Offers(read.Path[0]) {
							continue
						}
						errs = append(errs, field.Invalid(lf.fieldPath, text,
							contextUnavailableMessage(read.Path[0], surface, src.Name)))
					}
				}
			}
		}
	}
	return errs
}

// contextUnavailableMessage states the field, the surface that lacks it, why that
// surface is being mentioned, and where the field would work.
//
// The last clause is the one that decides what the author does next: move the
// consumption, or stop reading the field. Omitted when nothing offers it, since
// "available in" with an empty list reads as a bug.
func contextUnavailableMessage(field, surface, binding string) string {
	msg := fmt.Sprintf("context.%s is unavailable in %s, where source %q is consumed",
		field, sourceexpr.SurfacePlural(surface), binding)
	if available := sourceexpr.SurfacesOffering(field); len(available) > 0 {
		msg += "; it is available in " + strings.Join(available, ", ")
	}
	return msg
}
