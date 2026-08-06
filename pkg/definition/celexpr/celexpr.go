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

// Package celexpr is a spike: can CEL replace the hand-built expression language
// in pkg/definition/sourceexpr?
//
// The question is not whether CEL can evaluate - obviously it can - but whether
// it can carry the three things sourceexpr actually needs:
//
//  1. A *static* result type, derived before any value exists, to check against
//     the parameter the expression feeds. sourceexpr does this by materialising
//     the schema into sentinel values and evaluating, because CUE will not compute
//     on a non-concrete operand. CEL has a real type checker, so Compile() should
//     give the answer directly via OutputType().
//  2. A sandbox. sourceexpr walks the parsed AST and rejects anything whose result
//     type could depend on a value. CEL is sandboxed by construction - no I/O, no
//     imports, bounded evaluation - so the walk may become unnecessary.
//  3. The reads an expression makes, for dependency ordering and +sensitive
//     tracking. CEL exposes a walkable AST, so this should be recoverable.
//
// If those hold, the proprietary grammar goes away and conditionals become
// available for free: CEL's ternary already requires both arms to unify, which is
// the soundness rule we would have had to invent.
package celexpr

import (
	"fmt"
	"regexp"
	"strings"

	"cuelang.org/go/cue"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	apiservercel "k8s.io/apiserver/pkg/cel"
)

// DeclTypeFor converts a source's `schema:` block into a CEL type.
//
// This is the load-bearing step. A SourceDefinition declares its output contract
// in CUE; CEL needs a type declaration to check against. Kubernetes solves the
// same problem for CRD validation rules by going OpenAPI -> DeclType; this walks
// the CUE value directly, which skips a lossy hop through OpenAPI.
//
// Anything unrecognised becomes DynType rather than an error: an honest "I do not
// know this shape" that CEL will still evaluate, at the cost of static checking
// for that subtree. That mirrors how sourceexpr treats an open region.
func DeclTypeFor(v cue.Value) *apiservercel.DeclType {
	switch v.IncompleteKind() {
	case cue.StringKind:
		return apiservercel.StringType
	case cue.IntKind:
		return apiservercel.IntType
	case cue.FloatKind, cue.NumberKind:
		return apiservercel.DoubleType
	case cue.BoolKind:
		return apiservercel.BoolType
	case cue.ListKind:
		if elem := v.LookupPath(cue.MakePath(cue.AnyIndex)); elem.Exists() {
			return apiservercel.NewListType(DeclTypeFor(elem), -1)
		}
		return apiservercel.NewListType(apiservercel.AnyType, -1)
	case cue.StructKind:
		// An open map - `[string]: string` - is a CEL map. A closed struct with
		// named fields is a CEL object. The distinction matters: a map read may
		// be absent (has() applies), an object field is declared.
		if elem := v.LookupPath(cue.MakePath(cue.AnyString)); elem.Exists() {
			return apiservercel.NewMapType(apiservercel.StringType, DeclTypeFor(elem), -1)
		}
		fields := map[string]*apiservercel.DeclField{}
		iter, err := v.Fields(cue.Optional(true))
		if err != nil {
			return apiservercel.AnyType
		}
		for iter.Next() {
			name := iter.Selector().Unquoted()
			fields[name] = apiservercel.NewDeclField(
				name, DeclTypeFor(iter.Value()), !iter.IsOptional(), nil, nil)
		}
		return apiservercel.NewObjectType("source."+objectName(v), fields)
	default:
		return apiservercel.AnyType
	}
}

// objectName gives each schema object a distinct CEL type name. CEL requires
// object types to be named and registered, so two sources with different shapes
// cannot share one.
func objectName(v cue.Value) string {
	p := v.Path().String()
	if p == "" {
		return "root"
	}
	return p
}

// Env builds a CEL environment where `source` and `context` are typed from the
// schemas given, exactly as a surface would offer them.
//
// sources maps a binding name to its schema; ctx maps a context field to its
// type. Both become declared variables, so an undeclared read is a compile error
// rather than a runtime surprise - the same guarantee sourceexpr gets from its
// grammar walk, but from the type checker instead.
func Env(sources map[string]cue.Value, ctx map[string]*apiservercel.DeclType) (*cel.Env, error) {
	srcFields := map[string]*apiservercel.DeclField{}
	for name, schema := range sources {
		if err := ValidBindingName(name); err != nil {
			return nil, err
		}
		srcFields[name] = apiservercel.NewDeclField(name, DeclTypeFor(schema), true, nil, nil)
	}
	sourceType := apiservercel.NewObjectType("vela.source", srcFields)

	ctxFields := map[string]*apiservercel.DeclField{}
	for name, t := range ctx {
		ctxFields[name] = apiservercel.NewDeclField(name, t, true, nil, nil)
	}
	contextType := apiservercel.NewObjectType("vela.context", ctxFields)

	// DeclTypeProvider is what teaches the checker about the named object types
	// reachable from these roots. Registering them with cel.Types() is not enough:
	// the provider is what resolves `vela.source` when a field is selected off it.
	provider := apiservercel.NewDeclTypeProvider(sourceType, contextType)
	opts, err := provider.EnvOptions(types.NewEmptyRegistry())
	if err != nil {
		return nil, fmt.Errorf("declaring source and context types: %w", err)
	}
	opts = append(opts,
		cel.Variable("source", sourceType.CelType()),
		cel.Variable("context", contextType.CelType()),
	)
	return cel.NewEnv(opts...)
}

// bindingName is the shape a spec.sources[] entry's name must have to be
// readable in an expression.
var bindingName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidBindingName reports whether a binding can be named in an expression.
//
// A binding is read as a field selection - source.cfg.host - so its name has to
// be an identifier. A hyphen is the one that bites, because `source.cluster-info`
// parses as subtraction rather than as a name, and unlike CUE there is no bracket
// form to fall back on: `source` is an object type, not a map, so it cannot be
// indexed.
//
// This constrains only spec.sources[].name, which is a local alias the
// Application author picks. SourceDefinition names are untouched - they appear in
// spec.sources[].type and never inside an expression, so git-file and http-get
// stay as they are.
func ValidBindingName(name string) error {
	if bindingName.MatchString(name) {
		return nil
	}
	suggestion := strings.NewReplacer("-", "", ".", "", "/", "").Replace(name)
	return fmt.Errorf(
		"source binding name %q cannot be read in an expression: a binding is read as "+
			"source.%s, so the name must be a letter or underscore followed by letters, "+
			"digits or underscores; try %q",
		name, name, suggestion)
}

// collectTypes gathers every named object type reachable from a DeclType.
//
// Only object types are registered. Registering a primitive - int, string - is a
// conflict, because CEL already knows them; the checker only needs telling about
// the structs a field selection can resolve against.
func collectTypes(t *apiservercel.DeclType) []*apiservercel.DeclType {
	if t == nil {
		return nil
	}
	var out []*apiservercel.DeclType
	if len(t.Fields) > 0 {
		out = append(out, t)
		for _, f := range t.Fields {
			out = append(out, collectTypes(f.Type)...)
		}
	}
	if t.ElemType != nil && t.ElemType != t {
		out = append(out, collectTypes(t.ElemType)...)
	}
	return out
}

// OutputType compiles an expression and reports the type it produces.
//
// This is the whole point of the spike. sourceexpr needs sentinel values and an
// evaluation to answer this; CEL answers it from the AST, before any data exists.
func OutputType(env *cel.Env, expr string) (*cel.Type, error) {
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	return ast.OutputType(), nil
}

// Eval compiles and runs an expression against real data.
func Eval(env *cel.Env, expr string, in map[string]interface{}) (interface{}, error) {
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}
	prg, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("program: %w", err)
	}
	out, _, err := prg.Eval(in)
	if err != nil {
		return nil, err
	}
	return out.Value(), nil
}
