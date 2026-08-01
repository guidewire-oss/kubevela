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

package sourceexpr

import (
	"fmt"
	"sort"
)

// Fields lists the context fields this surface exposes, so a caller can pull
// exactly those out of its process context without knowing the schema's shape.
func (c ContextSchema) Fields() []string {
	out := make([]string, 0, len(c.types))
	for name := range c.types {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// HasExpression reports whether a decoded properties tree contains anything to
// substitute, so a caller can skip the work entirely when it does not.
func HasExpression(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		for _, nested := range t {
			if HasExpression(nested) {
				return true
			}
		}
	case []interface{}:
		for _, nested := range t {
			if HasExpression(nested) {
				return true
			}
		}
	case string:
		parsed, err := Parse(t)
		return err == nil && parsed.HasExpr()
	}
	return false
}

// EvalTree substitutes expressions throughout a decoded properties tree.
//
// Properties are arbitrary nested JSON, so this walks maps, slices and scalars
// alike. Anything containing no expression comes back unchanged, including the
// strings - a value with no delimiter is not rewritten.
func EvalTree(v interface{}, resolved map[string]map[string]interface{}, ctx map[string]interface{},
	schema ContextSchema, roots ...string) (interface{}, error) {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, nested := range t {
			value, err := EvalTree(nested, resolved, ctx, schema, roots...)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = value
		}
		return out, nil

	case []interface{}:
		out := make([]interface{}, len(t))
		for i, nested := range t {
			value, err := EvalTree(nested, resolved, ctx, schema, roots...)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = value
		}
		return out, nil

	case string:
		return EvalIn(t, resolved, ctx, schema, roots...)

	default:
		return v, nil
	}
}

// ValidateTree checks every expression in a properties tree without evaluating
// it, which is what admission needs: the values are not available there.
func ValidateTree(v interface{}, roots ...string) error {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, nested := range t {
			if err := ValidateTree(nested, roots...); err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
		}
	case []interface{}:
		for i, nested := range t {
			if err := ValidateTree(nested, roots...); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
	case string:
		parsed, err := Parse(t)
		if err != nil {
			return err
		}
		for _, f := range parsed.Fragments {
			if !f.IsExpr() {
				continue
			}
			if err := ValidateRoots(f.Expr, roots...); err != nil {
				return err
			}
		}
	}
	return nil
}
