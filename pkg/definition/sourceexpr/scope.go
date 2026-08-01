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
//
// It goes through JSON rather than ctx.Encode, and the reason is integers.
// Resolved source values arrive as JSON, so a schema's `int` is a Go float64 by
// the time it reaches here - and Encode turns that into a CUE *float*, which
// makes `maxReplicas div 2` fail with "invalid operands 6 and 2 to 'div' (type
// float and int)" while admission, typing against an int sentinel, said it was
// fine. Marshalling to JSON and compiling preserves 6 as an integer, so the two
// halves agree.
//
// This is still not text-building: json.Marshal escapes keys, so an
// attacker-controlled binding name remains a key and cannot become syntax. That
// property has its own regression test.
func buildScope(ctx *cue.Context, sources, contextValues map[string]interface{}) cue.Value {
	scope := map[string]interface{}{}
	if sources != nil {
		scope[SourceIdent] = sources
	}
	if contextValues != nil {
		scope[ContextIdent] = contextValues
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return ctx.Encode(scope)
	}
	return ctx.CompileBytes(raw)
}

// sentinelSources turns each source's schema into representative values.
//
// See sentinelFor for why the schema itself cannot be used: CUE will not compute
// on a non-concrete operand, so `region: string` leaves `region + "-x"` stuck.
func sentinelSources(ctx *cue.Context, schemas map[string]string, refs []Reference) (map[string]interface{}, error) {
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

	// A schema may declare an open map - labels: [string]: string - which has no
	// concrete field at any key, so a sentinel built from its fields alone leaves
	// source.s.labels["team"] reporting an undefined field. The keys actually
	// read are materialised individually, exactly as they are for context.
	for _, ref := range refs {
		if !ref.IsSource() || len(ref.Path) < 3 {
			continue
		}
		schema, ok := schemas[ref.Path[0]]
		if !ok {
			continue
		}
		v := ctx.CompileString(schema)
		if v.Err() != nil {
			continue
		}
		materialiseOpenMapKey(v, out[ref.Path[0]], ref.Path[1:])
	}
	return out, nil
}

// materialiseOpenMapKey adds a sentinel for a key read out of an open map,
// walking the schema and the sentinel in step.
func materialiseOpenMapKey(schema cue.Value, sentinel interface{}, path []string) {
	cur := schema
	node, ok := sentinel.(map[string]interface{})
	if !ok {
		return
	}
	for i, seg := range path {
		next := cur.LookupPath(cue.MakePath(cue.Str(seg)))
		if !next.Exists() {
			return
		}
		if i == len(path)-1 {
			return
		}
		// The segment after this one is a key into next; if next is an open map
		// it has no concrete field there, so supply one.
		if pattern := next.LookupPath(cue.MakePath(cue.AnyString)); pattern.Exists() {
			nested, _ := node[seg].(map[string]interface{})
			if nested == nil {
				nested = map[string]interface{}{}
				node[seg] = nested
			}
			value, err := sentinelFor(pattern)
			if err != nil {
				return
			}
			nested[path[i+1]] = value
			return
		}
		child, _ := node[seg].(map[string]interface{})
		if child == nil {
			return
		}
		node = child
		cur = next
	}
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
