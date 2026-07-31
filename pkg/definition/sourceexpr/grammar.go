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
	"strings"

	"cuelang.org/go/cue/ast"
	cueformat "cuelang.org/go/cue/format"
	"cuelang.org/go/cue/literal"
	cueparser "cuelang.org/go/cue/parser"
	"cuelang.org/go/cue/token"
)

// Validate accepts only expressions whose result type is a function of its
// operands' types, never of their values.
//
// That restriction is what makes admission-time type checking sound. Types are
// derived by evaluating the expression against representative sentinel values
// (see TypeOf), and a construct that branches on a value would be typed against
// the sentinel rather than against what happens at render:
//
//	[if source.scale.count > 5 {"big"}, "small"][0]
//
// types as a string with any sentinel, but so would `[..., 0][0]` type as int -
// the answer depends on data admission has not seen. Rejecting the construct is
// honest; guessing is not.
//
// It also holds the sandbox. A consumer expression may reach `source` and
// nothing else: no imports, no provider calls, no access to the definition's
// internals. That is narrower than what a SourceDefinition author may write, and
// deliberately so - the two operate at different trust levels.
func Validate(expr string) error {
	node, err := cueparser.ParseExpr("-", expr)
	if err != nil {
		return fmt.Errorf("not a valid expression: %w", err)
	}

	var bad error
	ast.Walk(node, func(n ast.Node) bool {
		if bad != nil {
			return false
		}
		switch t := n.(type) {
		case *ast.Comprehension:
			bad = fmt.Errorf("conditionals are not supported in a property expression: " +
				"the result type would depend on data not available when the Application is admitted")
		case *ast.IndexExpr:
			// A constant string index is field access by another name, and it is
			// the *required* form for a source whose name contains a hyphen -
			// source["my-source"].region - so it has to be allowed. Its result
			// type follows from the schema exactly as a selector's does.
			//
			// A numeric or computed index does not: the element type of a list is
			// not pinned by the sentinel, so typing it would be a guess.
			if lit, ok := t.Index.(*ast.BasicLit); !ok || lit.Kind != token.STRING {
				bad = fmt.Errorf("only a constant string index is supported in a property expression, " +
					"e.g. source[\"my-source\"].field")
			}
		case *ast.SliceExpr:
			bad = fmt.Errorf("slicing is not supported in a property expression")
		case *ast.ImportDecl, *ast.ImportSpec:
			bad = fmt.Errorf("imports are not allowed in a property expression: "+
				"an expression may reference %q and nothing else", SourceIdent)
		case *ast.BinaryExpr:
			switch t.Op {
			case token.LSS, token.GTR, token.LEQ, token.GEQ, token.EQL, token.NEQ:
				bad = fmt.Errorf("comparisons are not supported in a property expression")
			case token.OR:
				bad = fmt.Errorf("disjunctions are not supported in a property expression: " +
					"the result type would be ambiguous")
			case token.SUB:
				// A hyphenated source name written with dots parses as
				// subtraction, not as a path: source.my-source.region is
				// (source.my) - (source.region). Both halves are legal CUE, so
				// without this the author gets an "undefined field: my" from the
				// evaluator and no clue why.
				if hyphenHazard(t) {
					bad = fmt.Errorf("%q looks like a hyphenated source name written with dots, "+
						"which CUE parses as subtraction. Use bracket syntax: %s[\"<name>\"].<field>",
						exprText(t), SourceIdent)
				}
			}
		case *ast.Ident:
			// The sandbox. Every identifier is either the one permitted root or a
			// field name reached through it; a bare identifier is neither.
			if t.Name != SourceIdent && t.Name != ContextIdent && !isFieldPosition(node, t) {
				bad = fmt.Errorf("unknown identifier %q: an expression may reference %q and %q, nothing else",
					t.Name, SourceIdent, ContextIdent)
			}
		}
		return true
	}, nil)
	return bad
}

// isFieldPosition reports whether an identifier is a selector's field name
// (the `region` in source.s.region) rather than a value being referenced.
func isFieldPosition(root ast.Node, id *ast.Ident) bool {
	found := false
	ast.Walk(root, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if sel.Sel == ast.Label(id) || nodeIs(sel.Sel, id) {
				found = true
			}
		}
		return !found
	}, nil)
	return found
}

func nodeIs(label ast.Label, id *ast.Ident) bool {
	other, ok := label.(*ast.Ident)
	return ok && other == id
}

// Reference is one path an expression reads, rooted at source or context.
type Reference struct {
	// Root is SourceIdent or ContextIdent.
	Root string
	// Path is the rest: for a source, [binding, field...]; for context,
	// [field] or [field, index].
	Path []string
}

// IsSource reports whether the reference reads a resolved source.
func (r Reference) IsSource() bool { return r.Root == SourceIdent }

// String renders a reference the way an error message names it.
func (r Reference) String() string {
	return fmt.Sprintf("%s.%s", r.Root, strings.Join(r.Path, "."))
}

// References returns every source path the expression reads.
//
// Three things need this, which is why it is extracted rather than inferred at
// each use: admission validates each path against the source's schema; the
// binding's dependency order comes from which sources are named; and `+sensitive`
// taint propagates from these paths to the expression's result - "prefix-" plus a
// secret is not recognisably the secret, so the result has to inherit the marking
// rather than be re-detected.
func References(expr string) ([]Reference, error) {
	node, err := cueparser.ParseExpr("-", expr)
	if err != nil {
		return nil, fmt.Errorf("not a valid expression: %w", err)
	}

	seen := map[string]Reference{}
	ast.Walk(node, func(n ast.Node) bool {
		expr, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		switch expr.(type) {
		case *ast.SelectorExpr, *ast.IndexExpr:
		default:
			return true
		}
		root, path, ok := selectorPath(expr)
		if !ok || len(path) == 0 {
			return true
		}
		// A source needs a binding name and at least one field; context needs
		// only a field. A bare `source.x` is an incomplete read either way.
		if root == SourceIdent && len(path) < 2 {
			return true
		}
		ref := Reference{Root: root, Path: path}
		seen[ref.String()] = ref
		// Do not descend: the prefix of this chain is not a separate read.
		return false
	}, nil)

	out := make([]Reference, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// selectorPath flattens source.a.b (and source["a"].b) into ["a","b"], reporting
// the root identifier and whether the chain is rooted at a permitted one.
func selectorPath(head ast.Expr) (string, []string, bool) {
	root := ""
	var parts []string
	var walk func(ast.Expr) bool
	walk = func(e ast.Expr) bool {
		switch t := e.(type) {
		case *ast.Ident:
			if t.Name == SourceIdent || t.Name == ContextIdent {
				root = t.Name
				return true
			}
			return false
		case *ast.SelectorExpr:
			if !walk(t.X) {
				return false
			}
			name, _, err := ast.LabelName(t.Sel)
			if err != nil {
				return false
			}
			parts = append(parts, name)
			return true
		case *ast.IndexExpr:
			if !walk(t.X) {
				return false
			}
			lit, ok := t.Index.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return false
			}
			s, err := literal.Unquote(lit.Value)
			if err != nil {
				return false
			}
			parts = append(parts, s)
			return true
		}
		return false
	}
	if !walk(head) {
		return "", nil, false
	}
	return root, parts, true
}

// hyphenHazard reports a subtraction whose operands are both rooted at `source`,
// which is what a hyphenated name written with dots turns into.
func hyphenHazard(b *ast.BinaryExpr) bool {
	return rootedAtSource(b.X) && rootedAtSource(b.Y)
}

func rootedAtSource(e ast.Expr) bool {
	for {
		switch t := e.(type) {
		case *ast.Ident:
			return t.Name == SourceIdent || t.Name == ContextIdent
		case *ast.SelectorExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		default:
			return false
		}
	}
}

func exprText(e ast.Expr) string {
	b, err := cueformat.Node(e)
	if err != nil {
		return "the expression"
	}
	return string(b)
}
