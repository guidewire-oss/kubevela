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
)

// ValueType returns the type a whole property value will produce, which is what
// admission compares against the consuming parameter.
//
// TypeOf answers this for one expression. A property value is not always one
// expression: it may be plain text, a single expression, or several expressions
// interleaved with text. The parameter sees the finished value, so this is the
// function that has to line up with it.
//
//	"nginx:1.25"                   string  - no expression, a literal
//	"$(source.scale.replicas)"     int     - the expression's own type
//	"$(a.x) $(b.y)"                string  - concatenation is text
//	"port-$(source.scale.replicas)" string  - likewise
//
// A value with more than one expression, or with any surrounding text, is a
// string regardless of what its parts produce. That is knowable from the shape
// alone, without evaluating anything - so a parameter expecting an int can be
// told at admission that "$(a) $(b)" will never satisfy it.
func ValueType(raw string, schemas map[string]string) (cue.Kind, error) {
	return ValueTypeIn(raw, schemas, SourceIdent, ContextIdent)
}

// ValueTypeIn is ValueType restricted to the roots a surface permits, so a policy
// property is typed against context alone.
func ValueTypeIn(raw string, schemas map[string]string, roots ...string) (cue.Kind, error) {
	parsed, err := Parse(raw)
	if err != nil {
		return cue.BottomKind, err
	}
	if !parsed.HasExpr() {
		return cue.StringKind, nil
	}
	if expr, ok := parsed.SoleExpr(); ok {
		return TypeOfIn(expr, schemas, ComponentContext, roots...)
	}

	// Interleaved with text, so the result is a string. Every fragment is still
	// checked: an undeclared field or a bad operand is an error wherever it
	// appears, and a fragment that has no text form cannot be concatenated at
	// all. Catching that here is the point - otherwise it surfaces at render,
	// after the Application was admitted.
	for _, f := range parsed.Fragments {
		if !f.IsExpr() {
			continue
		}
		kind, err := TypeOfIn(f.Expr, schemas, ComponentContext, roots...)
		if err != nil {
			return cue.BottomKind, err
		}
		if !isTextual(kind) {
			return cue.BottomKind, fmt.Errorf("%s is a %s and cannot be combined with text; "+
				"reference a scalar field, or use the expression on its own so the value keeps its type",
				f.Expr, kind)
		}
	}
	return cue.StringKind, nil
}

// isTextual reports a kind with an unambiguous text form. A struct or a list has
// no single rendering, and choosing one silently would produce something the
// author did not ask for.
func isTextual(k cue.Kind) bool {
	switch k {
	case cue.StringKind, cue.IntKind, cue.FloatKind, cue.NumberKind, cue.BoolKind, cue.BytesKind:
		return true
	}
	return false
}
