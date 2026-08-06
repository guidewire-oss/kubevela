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
	"sort"
	"strings"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
)

// Reference is one read an expression makes, mirroring sourceexpr.Reference so
// the callers that depend on it do not have to change shape.
//
// Three things in the controller run off these, and all three fail quietly if the
// set under-approximates: which sources a component's render must resolve and in
// what order, whether a chained binding forms a cycle, and which resolved values
// are sensitive and must be redacted from status.
type Reference struct {
	// Root is "source" or "context".
	Root string
	// Path is the rest: for a source, [binding, field...]; for context, [field]
	// or [field, index].
	Path []string
	// Guarded records that the read sits behind a has() test or the false arm of
	// a ternary, so it survives the value being absent - the CEL equivalent of a
	// defaulted read.
	Guarded bool
}

// IsSource reports whether the reference reads a resolved source.
func (r Reference) IsSource() bool { return r.Root == "source" }

// String renders a reference the way an error message names it.
func (r Reference) String() string {
	if len(r.Path) == 0 {
		return r.Root
	}
	return r.Root + "." + strings.Join(r.Path, ".")
}

// References returns every read an expression makes.
//
// Walking the checked AST rather than the source text is what makes this exact:
// `source.cfg.data["image"]` and `source["cfg"].data.image` are the same read
// spelled two ways, and the parser has already normalised both into a select
// chain over an index.
//
// Reads inside a comprehension are included. A source read only in the untaken
// arm of a ternary is included too, deliberately: it still has to be resolved
// before the expression can be evaluated, and a value that might be substituted
// must count as sensitive whether or not this particular render reaches it.
func References(env *cel.Env, expr string) ([]Reference, error) {
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, iss.Err()
	}

	seen := map[string]Reference{}
	nav := celast.NavigateAST(ast.NativeRep())
	for _, n := range celast.MatchDescendants(nav, func(e celast.NavigableExpr) bool {
		// Only the outermost select of a chain: descending would also yield the
		// partial prefixes, so `source.cfg.meta.region` would report `source.cfg`
		// and `source.cfg.meta` alongside it.
		if e.Kind() != celast.SelectKind && e.Kind() != celast.CallKind {
			return false
		}
		return true
	}) {
		root, path, ok := chain(n)
		if !ok || (root != "source" && root != "context") {
			continue
		}
		r := Reference{Root: root, Path: path, Guarded: guarded(n)}
		if prev, dup := seen[r.String()]; !dup || (prev.Guarded && !r.Guarded) {
			seen[r.String()] = r
		}
	}

	out := make([]Reference, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return dropPrefixes(out), nil
}

// chain flattens a select/index chain into its root identifier and path.
//
// Returns ok=false for anything that is not a chain rooted at a plain
// identifier - a function result, a literal, a constructed map.
func chain(e celast.NavigableExpr) (string, []string, bool) {
	var path []string
	cur := e
	for {
		switch cur.Kind() {
		case celast.SelectKind:
			sel := cur.AsSelect()
			path = append([]string{sel.FieldName()}, path...)
			cur = sel.Operand().(celast.NavigableExpr)
		case celast.CallKind:
			call := cur.AsCall()
			// `a["b"]` is the _[_] operator, not a select.
			if call.FunctionName() != "_[_]" || len(call.Args()) != 2 {
				return "", nil, false
			}
			idx := call.Args()[1]
			if idx.Kind() != celast.LiteralKind {
				// A computed index cannot be named statically. Report the
				// container instead of dropping the read entirely.
				cur = call.Args()[0].(celast.NavigableExpr)
				continue
			}
			lit, ok := idx.AsLiteral().Value().(string)
			if !ok {
				return "", nil, false
			}
			path = append([]string{lit}, path...)
			cur = call.Args()[0].(celast.NavigableExpr)
		case celast.IdentKind:
			return cur.AsIdent(), path, true
		default:
			return "", nil, false
		}
	}
}

// guarded reports whether a read sits behind has() or inside a ternary arm, which
// is how CEL expresses "this may be absent and I have handled it".
func guarded(e celast.NavigableExpr) bool {
	for p, ok := e.Parent(); ok; p, ok = p.Parent() {
		if p.Kind() != celast.CallKind {
			continue
		}
		switch p.AsCall().FunctionName() {
		case "has", "_?_:_":
			return true
		}
	}
	return false
}

// dropPrefixes removes references that are a strict prefix of another.
//
// A chain yields its own prefixes as it is walked - `source.cfg.meta.region`
// also matches at `source.cfg.meta` and `source.cfg`. Only the deepest read is
// the one an author wrote, and it is the one the schema check must validate.
func dropPrefixes(in []Reference) []Reference {
	var out []Reference
	for i, r := range in {
		prefix := false
		for j, other := range in {
			if i == j || r.Root != other.Root || len(other.Path) <= len(r.Path) {
				continue
			}
			if strings.HasPrefix(other.String(), r.String()+".") {
				prefix = true
				break
			}
		}
		if !prefix {
			out = append(out, r)
		}
	}
	return out
}
