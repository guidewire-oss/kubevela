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
	"github.com/google/cel-go/common/operators"
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
		r := Reference{Root: root, Path: path, Guarded: guarded(n, root, path)}
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
func chain(e celast.NavigableExpr) (string, []string, bool) {
	return pathOf(e)
}

// guarded reports whether this specific read is defended against absence.
//
// Three things have to hold, and an earlier version of this checked only the
// last, which made it wrong in the unsafe direction:
//
//  1. The guard must test *this* path. `has(source.cfg.other) ? source.cfg.note
//     : "x"` defends nothing about note, and reading it still fails at render.
//  2. The read must sit in an arm, not in a condition - a condition is always
//     evaluated. Nesting matters: a read in the condition of an inner ternary is
//     unguarded even though that inner ternary sits in an outer one's arm.
//  3. Some enclosing construct must actually be a guard.
//
// Both `cond ? a : b` and `has(x) && ...` count, because CEL's logical operators
// absorb an error from one side when the other side settles the result.
func guarded(e celast.NavigableExpr, root string, path []string) bool {
	// A presence test on the read itself never fails.
	if e.Kind() == celast.SelectKind && e.AsSelect().IsTestOnly() {
		return true
	}
	child := celast.Expr(e)
	for p, ok := e.Parent(); ok; p, ok = p.Parent() {
		if p.Kind() == celast.CallKind {
			call := p.AsCall()
			args := call.Args()
			switch call.FunctionName() {
			case "_?_:_":
				if len(args) != 3 {
					break
				}
				// In the condition: not guarded by this ternary, and not by any
				// outer one either - the condition is evaluated regardless of
				// what encloses it.
				if args[0].ID() == child.ID() {
					return false
				}
				if testsPath(args[0], root, path) {
					return true
				}
			case "_&&_", "_||_":
				for _, a := range args {
					if a.ID() != child.ID() && testsPath(a, root, path) {
						return true
					}
				}
			}
		}
		child = p
	}
	return false
}

// testsPath reports whether an expression contains a presence test for this exact
// path - the thing that makes a read safe.
//
// CEL spells that two ways, and both are needed. has(x.y) covers a declared field,
// but its macro rejects an index: has(m["k"]) does not compile. A map key is
// therefore tested with `"k" in m`, and that is the only form available when the
// key is not an identifier - which is every domain-prefixed label,
// context.appLabels["platform.io/team"] among them.
func testsPath(e celast.Expr, root string, path []string) bool {
	found := false
	celast.PostOrderVisit(e, celast.NewExprVisitor(func(n celast.Expr) {
		if found {
			return
		}
		// has(x.y)
		if n.Kind() == celast.SelectKind && n.AsSelect().IsTestOnly() {
			if r, p, ok := pathOf(n); ok && r == root && samePath(p, path) {
				found = true
			}
			return
		}
		// "k" in m - the container plus the key is the path being tested.
		if n.Kind() == celast.CallKind {
			call := n.AsCall()
			if call.FunctionName() != operators.In && call.FunctionName() != operators.OldIn {
				return
			}
			args := call.Args()
			if len(args) != 2 || args[0].Kind() != celast.LiteralKind {
				return
			}
			key, ok := args[0].AsLiteral().Value().(string)
			if !ok {
				return
			}
			r, p, ok := pathOf(args[1])
			if ok && r == root && samePath(append(append([]string{}, p...), key), path) {
				found = true
			}
		}
	}))
	return found
}

func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pathOf flattens a select/index chain into its root identifier and path, over a
// plain Expr rather than a NavigableExpr, so it can be used inside a visitor.
func pathOf(e celast.Expr) (string, []string, bool) {
	var path []string
	cur := e
	for {
		switch cur.Kind() {
		case celast.SelectKind:
			sel := cur.AsSelect()
			path = append([]string{sel.FieldName()}, path...)
			cur = sel.Operand()
		case celast.CallKind:
			call := cur.AsCall()
			if call.FunctionName() != "_[_]" || len(call.Args()) != 2 {
				return "", nil, false
			}
			idx := call.Args()[1]
			if idx.Kind() != celast.LiteralKind {
				cur = call.Args()[0]
				continue
			}
			lit, ok := idx.AsLiteral().Value().(string)
			if !ok {
				return "", nil, false
			}
			path = append([]string{lit}, path...)
			cur = call.Args()[0]
		case celast.IdentKind:
			return cur.AsIdent(), path, true
		default:
			return "", nil, false
		}
	}
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

// UndefendedReads returns reads that may be absent at render and carry no guard.
//
// CEL's checker does not help here: `source.cfg.note` on an optional field
// compiles cleanly as a string and then fails at evaluation with "no such key".
// The same is true of any key of an open map. So the rule the CUE path enforces -
// a possibly-absent read feeding a *required* parameter must carry a default -
// has to be enforced the same way, from the AST rather than from the type.
//
// A read counts as defended when it sits under has() or in a ternary arm, which
// is how CEL spells "I have handled the absence".
//
// optional reports whether a path may be absent: an optional schema field, or any
// key of an open map. The caller supplies it because only the schema knows.
func UndefendedReads(env *cel.Env, expr string, optional func(Reference) bool) ([]Reference, error) {
	refs, err := References(env, expr)
	if err != nil {
		return nil, err
	}
	var out []Reference
	for _, r := range refs {
		if r.IsSource() && !r.Guarded && optional(r) {
			out = append(out, r)
		}
	}
	return out, nil
}
