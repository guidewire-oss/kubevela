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

// Package sourceexpr is a SPIKE. It explores replacing the fromSource directive
// with CUE expressions written inline in component properties:
//
//	properties:
//	  cluster: '$(source.clusterInfo.region + "-cluster")'
//
// The binding name is written with a dot, as above. A name that is not a legal
// CUE identifier - one containing a hyphen - needs bracket syntax instead,
// source["my-source"].region, because CUE parses the dotted form as subtraction.
// Validate says so explicitly rather than letting the evaluator complain about
// an undefined field.
//
// The motivating gap is that fromSource yields a whole value or nothing - there
// is no way to combine a resolved value with anything else. Today an author who
// needs "<region>-cluster" has to add a parameter to the SourceDefinition and
// build the string there, which puts a consumer's formatting concern inside a
// shared platform artifact.
//
// Nothing here is wired into resolution. See devlogs/2026-07-31-source-expressions.md
// for what is settled, what is not, and what it would cost.
package sourceexpr

import (
	"fmt"
	"strings"
)

// SourceIdent is the only identifier an expression may reference. Everything a
// consumer is allowed to read hangs off it, so the sandbox is "this name and
// nothing else" rather than a denylist.
const SourceIdent = "source"

const (
	open   = "$("
	escape = "$$("
)

// Fragment is one piece of a property value: either literal text or an
// expression to evaluate.
type Fragment struct {
	Text string
	// Expr is the expression source, without the surrounding $( ).
	Expr string
}

// IsExpr reports whether this fragment needs evaluating.
func (f Fragment) IsExpr() bool { return f.Expr != "" }

// Parsed is a property value broken into fragments.
type Parsed struct {
	Fragments []Fragment
}

// HasExpr reports whether the value contains anything to evaluate. A value with
// none is left exactly as it was.
func (p Parsed) HasExpr() bool {
	for _, f := range p.Fragments {
		if f.IsExpr() {
			return true
		}
	}
	return false
}

// Whole reports whether the value is a single expression and nothing else.
//
// This is what decides the substituted value's type. A whole value is replaced
// by the expression's typed result, so `replicas: '$(source["s"].count)'` yields
// an int even though YAML carried it as a string. An expression embedded in
// surrounding text can only produce a string, since that is what concatenation
// means. Terraform and Helm draw the same line.
//
// Surrounding whitespace does not count as text. Without that, a single trailing
// space - invisible in YAML and easy to leave behind when editing - would flip
// the substituted type from int to string, and the resulting mismatch would
// surface against the parameter, far from the space that caused it. Adding a
// visible character is a deliberate act; adding a space is not.
func (p Parsed) Whole() bool {
	seen := false
	for _, f := range p.Fragments {
		if f.IsExpr() {
			if seen {
				return false
			}
			seen = true
			continue
		}
		if strings.TrimSpace(f.Text) != "" {
			return false
		}
	}
	return seen
}

// SoleExpr returns the expression of a whole value. It is the expression
// fragment, which is not necessarily the first: leading whitespace is a text
// fragment and still leaves the value whole.
func (p Parsed) SoleExpr() (string, bool) {
	if !p.Whole() {
		return "", false
	}
	for _, f := range p.Fragments {
		if f.IsExpr() {
			return f.Expr, true
		}
	}
	return "", false
}

// Parse splits a property value into literal and expression fragments.
//
// `$$(` is a literal `$(`, so a value that genuinely contains the delimiter can
// still be written. Nesting is not supported: the expression ends at the first
// unbalanced `)`, counting parens so that `$(f((a)))` works.
func Parse(raw string) (Parsed, error) {
	var out Parsed
	var lit strings.Builder

	for i := 0; i < len(raw); {
		if strings.HasPrefix(raw[i:], escape) {
			lit.WriteString(open)
			i += len(escape)
			continue
		}
		if !strings.HasPrefix(raw[i:], open) {
			lit.WriteByte(raw[i])
			i++
			continue
		}

		end, err := closingParen(raw, i+len(open))
		if err != nil {
			return Parsed{}, err
		}
		if lit.Len() > 0 {
			out.Fragments = append(out.Fragments, Fragment{Text: lit.String()})
			lit.Reset()
		}
		expr := strings.TrimSpace(raw[i+len(open) : end])
		if expr == "" {
			return Parsed{}, fmt.Errorf("empty expression at offset %d", i)
		}
		out.Fragments = append(out.Fragments, Fragment{Expr: expr})
		i = end + 1
	}
	if lit.Len() > 0 {
		out.Fragments = append(out.Fragments, Fragment{Text: lit.String()})
	}
	return out, nil
}

// closingParen finds the ')' matching the '$(' that opened at start-2, ignoring
// parens inside string literals so that $(f(")")) is not mis-terminated.
func closingParen(raw string, start int) (int, error) {
	depth := 0
	var quote byte
	for i := start; i < len(raw); i++ {
		c := raw[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			if depth == 0 {
				return i, nil
			}
			depth--
		}
	}
	return 0, fmt.Errorf("unterminated expression: no closing ')' for the %q at offset %d", open, start-len(open))
}
