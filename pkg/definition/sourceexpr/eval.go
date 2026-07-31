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
	"encoding/json"
	"fmt"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

// Eval substitutes resolved source values into a property value.
//
// This is the render-time half of the same contract TypeOf checks at admission:
// the identical expression, evaluated against real values rather than sentinels.
// Two evaluations of one expression is the whole design - it is what lets a
// mismatch be refused before the Application is admitted rather than surfacing
// as a render failure later.
//
// A value that is a single expression is replaced by the typed result, so an int
// stays an int. An expression embedded in surrounding text yields a string,
// because that is what concatenating with text means.
func Eval(raw string, resolved map[string]map[string]interface{}, ctx map[string]interface{}) (interface{}, error) {
	parsed, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if !parsed.HasExpr() {
		// Rebuilt from fragments rather than returned as-is: Parse resolves the
		// $$( escape, and a value containing one still has to come back
		// unescaped even though there is nothing to evaluate.
		out := ""
		for _, f := range parsed.Fragments {
			out += f.Text
		}
		return out, nil
	}

	scope, err := valueScope(resolved)
	if err != nil {
		return nil, err
	}

	if parsed.Whole() {
		return evalFragment(scope, parsed.Fragments[0].Expr, ctx)
	}

	out := ""
	for _, f := range parsed.Fragments {
		if !f.IsExpr() {
			out += f.Text
			continue
		}
		value, err := evalFragment(scope, f.Expr, ctx)
		if err != nil {
			return nil, err
		}
		text, err := asText(value)
		if err != nil {
			return nil, fmt.Errorf("%q cannot be embedded in a string: %w", f.Expr, err)
		}
		out += text
	}
	return out, nil
}

func evalFragment(scope, expr string, ctx map[string]interface{}) (interface{}, error) {
	if err := Validate(expr); err != nil {
		return nil, err
	}
	refs, err := References(expr)
	if err != nil {
		return nil, err
	}
	ctxScope, err := contextValueScope(refs, ctx)
	if err != nil {
		return nil, err
	}
	v := cuecontext.New().CompileString(scope + "\n" + ctxScope + "\nout: " + expr + "\n")
	if v.Err() != nil {
		return nil, describe(v.Err())
	}
	out := v.LookupPath(cue.ParsePath("out"))
	if out.Err() != nil {
		return nil, describe(out.Err())
	}

	var decoded interface{}
	if err := out.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("evaluating %q: %w", expr, err)
	}
	return decoded, nil
}

// valueScope renders the resolved sources as CUE for the expression to read.
func valueScope(resolved map[string]map[string]interface{}) (string, error) {
	raw, err := marshalScope(resolved)
	if err != nil {
		return "", err
	}
	return SourceIdent + ": " + raw, nil
}

// marshalScope renders a Go map as CUE. JSON is valid CUE, so this needs no
// separate renderer - and going through JSON is what guarantees the values an
// expression sees are exactly the values that were resolved.
func marshalScope(v interface{}) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal expression scope: %w", err)
	}
	return string(raw), nil
}

// asText renders a value for embedding. Only scalars can be: a struct or list
// has no single obvious rendering, and picking one silently would produce
// something the author did not ask for.
func asText(v interface{}) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return fmt.Sprintf("%t", t), nil
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t)), nil
		}
		return fmt.Sprintf("%g", t), nil
	case int, int64:
		return fmt.Sprintf("%d", t), nil
	case nil:
		return "", fmt.Errorf("value is null")
	default:
		return "", fmt.Errorf("a %T has no single string form; reference a scalar field instead", v)
	}
}
