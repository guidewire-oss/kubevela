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

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	cueparser "cuelang.org/go/cue/parser"
)

// The scope an expression is evaluated against is built as a cue.Value from Go
// data, never assembled as CUE source text.
//
// That is a correctness property, not a style preference. Binding names and label
// keys come from the Application spec, so they are attacker-controlled if the
// author is hostile - a name like `a": {pwned: "yes"}, "b` concatenated into CUE
// text would inject fields into the scope, and the failure would be silent
// because the injected fields would simply be there. Encoding the data means a
// name can only ever be a key: there is no syntax for it to escape into.

// evalIn evaluates an expression against a scope and returns the result.
//
// The scope is supplied through cue.Scope rather than prepended to the source,
// which is what keeps the data out of the text. The expression itself is still
// concatenated into `out: <expr>`, so it is re-parsed here as a single
// expression first: a value like "x\nevil: 1" would otherwise add a field to the
// compiled file. Callers do validate before reaching this, but relying on call
// order for that would make the guarantee one refactor from gone.
func evalIn(ctx *cue.Context, scope cue.Value, expr string) (cue.Value, error) {
	if scope.Err() != nil {
		return cue.Value{}, scope.Err()
	}
	if _, err := cueparser.ParseExpr("-", expr); err != nil {
		return cue.Value{}, fmt.Errorf("not a single expression: %w", err)
	}
	v := ctx.CompileString("out: "+expr, cue.Scope(scope))
	if v.Err() != nil {
		return cue.Value{}, describe(v.Err())
	}
	out := v.LookupPath(cue.ParsePath("out"))
	if out.Err() != nil {
		return cue.Value{}, describe(out.Err())
	}
	return out, nil
}

// buildScope encodes the two readable roots into a single value.
func buildScope(ctx *cue.Context, sources, contextValues map[string]interface{}) cue.Value {
	scope := map[string]interface{}{}
	if sources != nil {
		scope[SourceIdent] = sources
	}
	if contextValues != nil {
		scope[ContextIdent] = contextValues
	}
	return ctx.Encode(scope)
}

// sentinelSources turns each source's schema into representative values.
//
// See sentinelFor for why the schema itself cannot be used: CUE will not compute
// on a non-concrete operand, so `region: string` leaves `region + "-x"` stuck.
func sentinelSources(ctx *cue.Context, schemas map[string]string) (map[string]interface{}, error) {
	out := make(map[string]interface{}, len(schemas))
	for name, schema := range schemas {
		v := ctx.CompileString(schema)
		if v.Err() != nil {
			return nil, fmt.Errorf("source %q has an unreadable schema: %w", name, v.Err())
		}
		sentinel, err := sentinelFor(v)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", name, err)
		}
		out[name] = sentinel
	}
	return out, nil
}

// sentinelFor renders one schema value as a concrete sentinel of the same kind.
//
// The choices are not arbitrary. An int sentinel of 0 makes `100 / source.x.n`
// fail admission with a division-by-zero that would never happen at render, so
// the sentinel is 1. A string sentinel is non-empty for the same reason: an
// empty string is the degenerate case for most string operations, and typing
// should not sit on a degenerate input.
func sentinelFor(v cue.Value) (interface{}, error) {
	switch v.IncompleteKind() {
	case cue.StringKind:
		return "x", nil
	case cue.IntKind:
		return 1, nil
	case cue.NumberKind, cue.FloatKind:
		return 1.0, nil
	case cue.BoolKind:
		return true, nil
	case cue.BytesKind:
		return []byte("x"), nil
	case cue.ListKind:
		// An empty list types as a list without committing to an element type,
		// which is all that is needed while list indexing is rejected.
		return []interface{}{}, nil
	case cue.StructKind:
		iter, err := v.Fields(cue.Optional(true))
		if err != nil {
			return nil, err
		}
		out := map[string]interface{}{}
		for iter.Next() {
			nested, err := sentinelFor(iter.Value())
			if err != nil {
				return nil, err
			}
			out[iter.Selector().Unquoted()] = nested
		}
		return out, nil
	default:
		return nil, fmt.Errorf("cannot represent a %s field", v.IncompleteKind())
	}
}

// newContext returns the CUE context an evaluation runs in.
//
// One per call: a cue.Value belongs to the context that made it, so the scope
// and the expression have to share one.
func newContext() *cue.Context { return cuecontext.New() }
