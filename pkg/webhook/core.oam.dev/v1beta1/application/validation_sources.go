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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/oam"
)

type fromSourceReference struct {
	SourceName       string
	Path             string
	FieldPath        *field.Path
	FromSourceObject bool
	SourceIndex      int
	// HasDefault records whether the fromSource selector supplied a default:
	// value (only possible with the map form). Used to enforce that an optional
	// schema field consumed without a default is rejected at admission.
	HasDefault bool
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
		refs = append(refs, compRefs...)
		for j, tr := range comp.Traits {
			trRefs, trErrs := collectFromRawExtension(tr.Properties, field.NewPath("spec", "components").Index(i).Child("traits").Index(j).Child("properties"), -1)
			errs = append(errs, trErrs...)
			refs = append(refs, trRefs...)
		}
	}
	for i, policy := range app.Spec.Policies {
		policyRefs, policyErrs := collectFromRawExtension(policy.Properties, field.NewPath("spec", "policies").Index(i).Child("properties"), -1)
		errs = append(errs, policyErrs...)
		refs = append(refs, policyRefs...)
	}
	if app.Spec.Workflow != nil {
		for i, step := range app.Spec.Workflow.Steps {
			stepRefs, stepErrs := collectFromRawExtension(step.Properties, field.NewPath("spec", "workflow", "steps").Index(i).Child("properties"), -1)
			errs = append(errs, stepErrs...)
			refs = append(refs, stepRefs...)
			for j, sub := range step.SubSteps {
				subRefs, subErrs := collectFromRawExtension(sub.Properties, field.NewPath("spec", "workflow", "steps").Index(i).Child("subSteps").Index(j).Child("properties"), -1)
				errs = append(errs, subErrs...)
				refs = append(refs, subRefs...)
			}
		}
	}
	for i, src := range app.Spec.Sources {
		srcRefs, srcErrs := collectFromRawExtension(src.Properties, field.NewPath("spec", "sources").Index(i).Child("properties"), i)
		errs = append(errs, srcErrs...)
		refs = append(refs, srcRefs...)
	}

	schemaValidators := map[string]*sourceSchemaValidator{}
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
		if sourceType == "" {
			continue
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
		// KEP-2.16: an optional schema field (field?:) consumed without a
		// default: may be absent at runtime, leaving the target unresolvable.
		// Reject unless the map form supplies a default:.
		if validator.IsOptionalPath(ref.Path) && !ref.HasDefault {
			errs = append(errs, field.Invalid(ref.FieldPath, ref.Path,
				fmt.Sprintf("path %q is an optional field in schema of SourceDefinition %q; a default must be supplied via the fromSource map form", ref.Path, sourceType)))
		}
	}

	return errs
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
			name, sourcePath, hasDefault, err := parseFromSourceSelector(selector)
			if err != nil {
				errs = append(errs, field.Invalid(path.Child("fromSource"), selector, err.Error()))
				return refs, errs
			}
			refs = append(refs, fromSourceReference{
				SourceName:  name,
				Path:        sourcePath,
				FieldPath:   path.Child("fromSource"),
				SourceIndex: sourceIndex,
				HasDefault:  hasDefault,
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
		if err != nil || name != "schema" {
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
