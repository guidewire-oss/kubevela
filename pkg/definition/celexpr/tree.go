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

package celexpr

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"

	"github.com/oam-dev/kubevela/pkg/definition/sourceexpr"
)

// A property blob is arbitrary JSON, and an expression can sit at any depth
// inside it - in a nested object, in a list entry, in a k8s-objects manifest.
// These walk it.
//
// The walking is not specific to the expression language; only what happens at a
// string leaf is. sourceexpr.Parse still does the `$( )` splitting, because that
// is text handling rather than evaluation.

// ValidateTree checks every expression in a property blob, refusing any that does
// not compile or that reads a root this surface does not permit.
//
// The permissive environment is deliberate: this is the grammar-level pass, run
// before schemas are loaded. Types are checked separately, against the declared
// shapes.
func ValidateTree(v interface{}, roots ...string) error {
	env, err := DynEnv()
	if err != nil {
		return err
	}
	return validateNode(env, v, roots)
}

func validateNode(env *cel.Env, v interface{}, roots []string) error {
	switch t := v.(type) {
	case map[string]interface{}:
		for _, child := range t {
			if err := validateNode(env, child, roots); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range t {
			if err := validateNode(env, child, roots); err != nil {
				return err
			}
		}
	case string:
		parsed, err := sourceexpr.Parse(t)
		if err != nil || !parsed.HasExpr() {
			return err
		}
		for _, f := range parsed.Fragments {
			if !f.IsExpr() {
				continue
			}
			refs, rerr := References(env, f.Expr)
			if rerr != nil {
				return rerr
			}
			for _, r := range refs {
				if !contains(roots, r.Root) {
					return fmt.Errorf("%q cannot be read here; this surface permits %q",
						r.Root, strings.Join(roots, `", "`))
				}
			}
		}
	}
	return nil
}

// EvalTree substitutes every expression in a property blob.
//
// A leaf that is a single expression keeps its type - an int stays an int - and
// one embedded in text becomes a string. A leaf with no expression is returned
// byte-identical, so a plain value is never rewritten.
func EvalTree(v interface{}, resolved map[string]map[string]interface{},
	ctx map[string]interface{}) (interface{}, error) {
	env, err := DynEnv()
	if err != nil {
		return nil, err
	}
	sources := map[string]interface{}{}
	for name, values := range resolved {
		sources[name] = values
	}
	in := map[string]interface{}{"source": sources, "context": ctx}
	return evalNode(env, v, in)
}

func evalNode(env *cel.Env, v interface{}, in map[string]interface{}) (interface{}, error) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			out, err := evalNode(env, child, in)
			if err != nil {
				return nil, err
			}
			t[k] = out
		}
		return t, nil
	case []interface{}:
		for i, child := range t {
			out, err := evalNode(env, child, in)
			if err != nil {
				return nil, err
			}
			t[i] = out
		}
		return t, nil
	case string:
		return EvalProperty(env, t, in)
	default:
		return v, nil
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
