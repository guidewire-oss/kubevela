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
	"strconv"
	"strings"

	"slices"

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
	return ValidateRoots(expr, SourceIdent, ContextIdent)
}

// ValidateRoots is Validate restricted to a subset of the readable roots.
//
// It exists because a surface can support one root and not the other. An
// Application-scoped policy renders before the appfile exists
// (application_controller.go orders the transforms ahead of GenerateAppFile), so
// there is no parsed spec.sources[] to resolve against - but `context` is built
// by hand for that render and is perfectly available. Permitting context and
// withholding source lets such a surface carry expressions at all, instead of
// being excluded because half the feature cannot work there.
//
// Restricting at validation is what makes that honest: reading `source` on a
// surface that cannot resolve it is rejected with a reason, rather than
// substituted with nothing or left inert.
func ValidateRoots(expr string, allowed ...string) error {
	node, err := cueparser.ParseExpr("-", expr)
	if err != nil {
		return fmt.Errorf("not a valid expression: %w", err)
	}
	permitted := func(name string) bool { return slices.Contains(allowed, name) }

	// Default markers are legal only in the one position isDefaultedRead
	// recognises, so they are collected first and checked against below.
	defaultMarkers := map[*ast.UnaryExpr]bool{}
	ast.Walk(node, func(n ast.Node) bool {
		if b, ok := n.(*ast.BinaryExpr); ok && b.Op == token.OR {
			if u, ok := b.X.(*ast.UnaryExpr); ok && u.Op == token.MUL {
				defaultMarkers[u] = true
			}
		}
		return true
	}, nil)

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
			// A constant integer index into a list is sound for the same reason:
			// `outputs: [...{kind: string}]` pins the element type, so
			// outputs[0].kind types exactly as a field does. What the schema does
			// *not* pin is the list's length, so an indexed read is treated as
			// possibly-absent - see optionalPath - and needs a default when it
			// feeds a required parameter, exactly as context.appLabels["x"] does.
			//
			// A computed index is still refused: its result would depend on data
			// that does not exist when the Application is admitted.
			if lit, ok := t.Index.(*ast.BasicLit); !ok || (lit.Kind != token.STRING && lit.Kind != token.INT) {
				bad = fmt.Errorf("only a constant string or integer index is supported in a property " +
					"expression, e.g. source[\"my-source\"].field or source.s.list[0]")
			}
		case *ast.SliceExpr:
			bad = fmt.Errorf("slicing is not supported in a property expression")
		case *ast.UnaryExpr:
			// The default marker is meaningful only as the left side of the
			// disjunction above; anywhere else it is a no-op that would read as
			// if it did something.
			if t.Op == token.MUL && !defaultMarkers[t] {
				bad = fmt.Errorf("the default marker * is only meaningful in a default, " +
					"written *<value> | <fallback>")
			}
		case *ast.ImportDecl, *ast.ImportSpec:
			bad = fmt.Errorf("imports are not allowed in a property expression: "+
				"an expression may reference %q and nothing else", SourceIdent)
		case *ast.BinaryExpr:
			switch t.Op {
			case token.LSS, token.GTR, token.LEQ, token.GEQ, token.EQL, token.NEQ:
				bad = fmt.Errorf("comparisons are not supported in a property expression")
			case token.OR:
				// One disjunction is allowed, and it is the one that gives parity
				// with fromSource's `default:` - a fallback for a value that is
				// absent at render:
				//
				//	*source.img.image | "nginx:latest"
				//
				// The default marker has to sit on the *value*, not the fallback.
				// `x | *"d"` yields the default even when x is present, and a bare
				// `x | "d"` is ambiguous rather than a fallback - both silently
				// wrong, which is why the shape is checked rather than trusted.
				//
				// It stays soundly typeable because neither branch is chosen by a
				// value: the result is the value's type when present and the
				// default's when not, and TypeOf rejects the case where those
				// differ.
				if !isDefaultedRead(t) {
					bad = fmt.Errorf("the only disjunction allowed in a property expression is a default, "+
						"written with the marker on the value: *%s.<name>.<field> | <fallback>", SourceIdent)
				}
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
			// The sandbox. Every identifier is either a permitted root or a field
			// name reached through one; a bare identifier is neither.
			switch {
			case permitted(t.Name):
			case assertedTypes[t.Name]:
				// A builtin type name, for asserting the type of a read out of an
				// open field. Not a sandbox escape: it names a type, and a bare
				// one is non-concrete and fails on its own merits.
			case isFieldPosition(node, t):
			case t.Name == SourceIdent || t.Name == ContextIdent:
				bad = fmt.Errorf("%q cannot be read here; this surface permits %s",
					t.Name, strings.Join(quoteAll(allowed), " and "))
			default:
				bad = fmt.Errorf("unknown identifier %q: an expression may reference %s, nothing else",
					t.Name, strings.Join(quoteAll(allowed), " and "))
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
	// Defaulted records that the read carries a fallback - *read | literal - so
	// it survives the value being absent.
	Defaulted bool
}

// IsSource reports whether the reference reads a resolved source.
func (r Reference) IsSource() bool { return r.Root == SourceIdent }

// String renders a reference the way an error message names it.
//
// Segments are rendered so the result is a valid expression, because the errors
// that use it tell the author what to write instead - "supply a default with
// *<ref> | <fallback>". A path joined with dots would suggest
// `source.cfg.outputs.0.name`, which does not parse, and the author would be
// left correcting the suggestion before they could take it.
func (r Reference) String() string {
	var b strings.Builder
	b.WriteString(r.Root)
	for _, segment := range r.Path {
		switch {
		case isIndexSegment(segment):
			fmt.Fprintf(&b, "[%s]", segment)
		case isCUEIdent(segment):
			b.WriteString("." + segment)
		default:
			// A hyphenated source name or a label key with a dot in it. Bracket
			// syntax is the only form that reads these at all.
			fmt.Fprintf(&b, "[%q]", segment)
		}
	}
	return b.String()
}

// isIndexSegment reports a segment that came from a list index. selectorPath
// records those as decimal text, and nothing else in a path is all digits: a
// struct field cannot start with one.
func isIndexSegment(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isCUEIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '$':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
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

	// Reads sitting under a default marker survive being absent, so they are
	// collected before the walk that records them.
	defaulted := map[ast.Expr]bool{}
	ast.Walk(node, func(n ast.Node) bool {
		if b, ok := n.(*ast.BinaryExpr); ok && b.Op == token.OR {
			if u, ok := b.X.(*ast.UnaryExpr); ok && u.Op == token.MUL {
				// The marker may sit over an asserted read - *(x & string) | "d" -
				// so the read itself is what gets recorded, not the wrapper. Miss
				// this and the read is reported as needing a default it already has.
				if read, ok := readUnderMarker(u.X); ok {
					defaulted[read] = true
				}
			}
		}
		return true
	}, nil)

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
		ref := Reference{Root: root, Path: path, Defaulted: defaulted[expr]}
		// A path read twice is defaulted only if every read of it is; the
		// undefended one is what would fail.
		if prev, ok := seen[ref.String()]; ok && !prev.Defaulted {
			ref.Defaulted = false
		}
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
			if !ok {
				return false
			}
			switch lit.Kind {
			case token.STRING:
				s, err := literal.Unquote(lit.Value)
				if err != nil {
					return false
				}
				parts = append(parts, s)
			case token.INT:
				// The index joins the path as its decimal text. That reads as
				// ambiguous with a map key of the same name, and is not: whether a
				// segment is an index or a field is decided by what the schema
				// holds at that point, and a CUE value cannot be both a list and a
				// struct. Every walker below asks the schema rather than the string.
				if _, err := strconv.Atoi(lit.Value); err != nil {
					return false
				}
				parts = append(parts, lit.Value)
			default:
				return false
			}
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

// isDefaultedRead recognises `*<read> | <literal>`: a value with a fallback for
// when it is absent at render.
//
// The fallback must be a literal. Allowing an arbitrary expression there would
// mean the result type could come from either branch by a route the type checker
// cannot follow, and a fallback that itself needs computing is a sign the author
// wants something this grammar deliberately does not have.
func isDefaultedRead(b *ast.BinaryExpr) bool {
	u, ok := b.X.(*ast.UnaryExpr)
	if !ok || u.Op != token.MUL {
		return false
	}
	if !isReadOrAssertedRead(u.X) {
		return false
	}
	switch b.Y.(type) {
	case *ast.BasicLit:
		return true
	}
	return false
}

// readUnderMarker digs the read out from under a default marker, past the
// parentheses and the type assertion it may be wrapped in.
func readUnderMarker(e ast.Expr) (ast.Expr, bool) {
	if p, ok := e.(*ast.ParenExpr); ok {
		e = p.X
	}
	switch t := e.(type) {
	case *ast.SelectorExpr, *ast.IndexExpr:
		return e, true
	case *ast.BinaryExpr:
		if t.Op != token.AND {
			return nil, false
		}
		for _, pair := range [][2]ast.Expr{{t.X, t.Y}, {t.Y, t.X}} {
			read, typ := pair[0], pair[1]
			id, ok := typ.(*ast.Ident)
			if !ok || !assertedTypes[id.Name] {
				continue
			}
			switch read.(type) {
			case *ast.SelectorExpr, *ast.IndexExpr:
				return read, true
			}
		}
	}
	return nil, false
}

// isReadOrAssertedRead accepts a read, or a read with a type asserted onto it.
//
// Both have to be allowed under a default marker, because the two conditions
// they answer are independent: an open field needs an assertion to have a type
// at all, and a value that may be absent needs a fallback. A field that is both
// - `outputs: [string]: _`, where the key may not exist and its type is not
// declared - has no other way to be written:
//
//	*(source.cfg.outputs.settings.data.region & string) | "unknown"
func isReadOrAssertedRead(e ast.Expr) bool {
	if p, ok := e.(*ast.ParenExpr); ok {
		e = p.X
	}
	switch t := e.(type) {
	case *ast.SelectorExpr, *ast.IndexExpr:
		return true
	case *ast.BinaryExpr:
		if t.Op != token.AND {
			return false
		}
		// Either order: `x & string` and `string & x` mean the same thing, which
		// is what Assertions already accepts.
		for _, pair := range [][2]ast.Expr{{t.X, t.Y}, {t.Y, t.X}} {
			read, typ := pair[0], pair[1]
			id, ok := typ.(*ast.Ident)
			if !ok || !assertedTypes[id.Name] {
				continue
			}
			switch read.(type) {
			case *ast.SelectorExpr, *ast.IndexExpr:
				return true
			}
		}
	}
	return false
}

func quoteAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, fmt.Sprintf("%q", n))
	}
	return out
}

// assertedType is a CUE builtin type an expression may assert onto a read.
//
// Only these: a read out of an open field has no declared type, and the point of
// the assertion is to supply one that admission can check the target against. A
// struct or list assertion would say almost nothing, so it is not offered.
var assertedTypes = map[string]bool{
	"string": true,
	"int":    true,
	"float":  true,
	"number": true,
	"bool":   true,
	"bytes":  true,
}

// Assertions returns the type each read in an expression asserts for itself,
// keyed by the reference's string form.
//
// `source.cfg.content.replicas & int` says "whatever this file holds here, treat
// it as an int". That is the only way to type a read out of a `_` field, so it is
// how a generic source's consumer declares what they expect - and it is checked
// at render, where the real value either unifies with the asserted type or fails
// loudly.
func Assertions(expr string) (map[string]string, error) {
	node, err := cueparser.ParseExpr("-", expr)
	if err != nil {
		return nil, fmt.Errorf("not a valid expression: %w", err)
	}

	out := map[string]string{}
	ast.Walk(node, func(n ast.Node) bool {
		b, ok := n.(*ast.BinaryExpr)
		if !ok || b.Op != token.AND {
			return true
		}
		// Either order: `x & string` and `string & x` mean the same thing.
		for _, pair := range [][2]ast.Expr{{b.X, b.Y}, {b.Y, b.X}} {
			read, typ := pair[0], pair[1]
			id, ok := typ.(*ast.Ident)
			if !ok || !assertedTypes[id.Name] {
				continue
			}
			root, path, ok := selectorPath(read)
			if !ok || len(path) == 0 {
				continue
			}
			out[Reference{Root: root, Path: path}.String()] = id.Name
		}
		return true
	}, nil)
	return out, nil
}

// AssertedKinds names the types this package accepts in an assertion, for an
// error message.
func AssertedKinds() []string {
	out := make([]string, 0, len(assertedTypes))
	for name := range assertedTypes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
