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
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
)

// TypeOf returns the type an expression will produce, given the schemas of the
// sources it reads. It is the admission-time half of the contract: the result is
// checked against the consuming parameter's type before the Application is
// admitted, so a mismatch is refused rather than discovered at render.
//
// The obvious implementation does not work. Substituting the schema itself -
// `region: string` - and evaluating leaves CUE with a non-concrete operand:
//
//	out: source.clusterInfo.region + "-cluster"
//	=> non-concrete value string in operand to +
//
// CUE will not compute on a type. So the schema is materialised into concrete
// sentinel values of the right kind, the expression is evaluated against those,
// and the result's kind is the answer. The schema still supplies the types; the
// sentinel is only what makes the evaluator willing to run.
//
// This is sound exactly because Validate rejects everything whose result type
// could depend on a value. Call Validate first.
func TypeOf(expr string, schemas map[string]string) (cue.Kind, error) {
	if err := Validate(expr); err != nil {
		return cue.BottomKind, err
	}

	refs, err := References(expr)
	if err != nil {
		return cue.BottomKind, err
	}
	scope, err := sentinelScope(schemas)
	if err != nil {
		return cue.BottomKind, err
	}
	ctxScope, err := contextScope(refs)
	if err != nil {
		return cue.BottomKind, err
	}

	v := cuecontext.New().CompileString(scope + "\n" + ctxScope + "\nout: " + expr + "\n")
	if v.Err() != nil {
		return cue.BottomKind, describe(v.Err())
	}
	out := v.LookupPath(cue.ParsePath("out"))
	if out.Err() != nil {
		return cue.BottomKind, describe(out.Err())
	}

	kind := out.IncompleteKind()
	if !isSingleKind(kind) {
		// The one way to reach this is a default whose type differs from the
		// value it falls back for - `*source.s.count | "none"` types as
		// (int|string). A parameter cannot be two types, so the author is told
		// here rather than at render, where only one branch would be taken and
		// the mismatch might not show for months.
		return cue.BottomKind, fmt.Errorf("this expression could be %s depending on whether the value "+
			"is present; a default must have the same type as the value it replaces", kind)
	}
	return kind, nil
}

// isSingleKind reports a kind that names exactly one type.
//
// cue.Kind is a bitmask, so a disjunction of two types shows up as both bits
// set. NumberKind is the exception worth allowing: it is int|float by
// definition, not an ambiguity.
func isSingleKind(k cue.Kind) bool {
	if k == cue.NumberKind {
		return true
	}
	return k != 0 && k&(k-1) == 0
}

// sentinelScope renders `source: {...}` with every schema field replaced by a
// representative value of its declared kind.
func sentinelScope(schemas map[string]string) (string, error) {
	ctx := cuecontext.New()
	var b strings.Builder
	b.WriteString(SourceIdent + ": {\n")

	for name, schema := range schemas {
		v := ctx.CompileString(schema)
		if v.Err() != nil {
			return "", fmt.Errorf("source %q has an unreadable schema: %w", name, v.Err())
		}
		rendered, err := sentinelFor(v)
		if err != nil {
			return "", fmt.Errorf("source %q: %w", name, err)
		}
		fmt.Fprintf(&b, "  %q: %s\n", name, rendered)
	}
	b.WriteString("}\n")
	return b.String(), nil
}

// sentinelFor renders one value as a concrete sentinel of the same kind.
//
// The choices are not arbitrary. An int sentinel of 0 makes `100 / source.x.n`
// fail admission with a division-by-zero that would never happen at render, so
// the sentinel is 1. A string sentinel is non-empty for the same reason: an
// empty string is the degenerate case for most string operations, and typing
// should not sit on a degenerate input.
func sentinelFor(v cue.Value) (string, error) {
	switch v.IncompleteKind() {
	case cue.StringKind:
		return `"x"`, nil
	case cue.IntKind:
		return "1", nil
	case cue.NumberKind, cue.FloatKind:
		return "1.0", nil
	case cue.BoolKind:
		return "true", nil
	case cue.BytesKind:
		return `'x'`, nil
	case cue.ListKind:
		// An empty list types as a list without committing to an element type,
		// which is all that is needed while indexing is rejected.
		return "[]", nil
	case cue.StructKind:
		iter, err := v.Fields(cue.Optional(true))
		if err != nil {
			return "", err
		}
		var b strings.Builder
		b.WriteString("{")
		first := true
		for iter.Next() {
			nested, err := sentinelFor(iter.Value())
			if err != nil {
				return "", err
			}
			if !first {
				b.WriteString(", ")
			}
			first = false
			fmt.Fprintf(&b, "%q: %s", iter.Selector().Unquoted(), nested)
		}
		b.WriteString("}")
		return b.String(), nil
	default:
		return "", fmt.Errorf("cannot represent a %s field", v.IncompleteKind())
	}
}

// describe turns CUE's evaluator errors into something a user can act on. The
// raw text mentions sentinel values they never wrote, which reads as nonsense
// without the translation.
func describe(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "undefined field"):
		return fmt.Errorf("%w - the field is not declared in the source's schema", err)
	case strings.Contains(msg, "invalid operands"):
		return fmt.Errorf("%w - check the types the source's schema declares", err)
	}
	return err
}
