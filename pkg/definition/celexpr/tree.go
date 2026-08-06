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
	"math"
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

// normaliseNumbers turns an integral float64 into an int64 throughout a value.
//
// Resolved source values reach here having been through JSON, where every number
// decodes as float64 and the int/float distinction is lost. CEL has no mixed
// numeric overloads, so `source.cfg.port + 1000` on a JSON-decoded 8080 fails at
// evaluation with "no such overload" - an error that names neither the field nor
// the cause, on an expression that type-checked cleanly at admission because the
// schema said `port: int`.
//
// Restoring the distinction from the value is the best available reading: a JSON
// number with no fractional part was an integer in the schema that produced it.
// The cost is a float-typed field holding exactly 2.0, which becomes an int and
// then needs double() before it can be used in float arithmetic. That is the rarer
// case by a wide margin, and unlike the alternative it is expressible.
func normaliseNumbers(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, child := range t {
			out[k] = normaliseNumbers(child)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, child := range t {
			out[i] = normaliseNumbers(child)
		}
		return out
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return int64(t)
		}
		return t
	case float32:
		return normaliseNumbers(float64(t))
	case int:
		return int64(t)
	default:
		return v
	}
}

// normaliseInput applies normaliseNumbers to an activation map, keeping the type
// CEL's Eval expects.
func normaliseInput(in map[string]interface{}) map[string]interface{} {
	out, _ := normaliseNumbers(in).(map[string]interface{})
	if out == nil {
		return in
	}
	return out
}
