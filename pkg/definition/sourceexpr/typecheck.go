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
	return TypeOfIn(expr, schemas, ComponentContext, SourceIdent, ContextIdent)
}

// TypeOfIn is TypeOf against a named surface: its context schema, and the roots
// it permits.
//
// Both vary by surface. An Application-scoped policy has a smaller context and
// cannot read sources at all, so typing an expression there against the component
// schema would accept fields that surface never receives.
func TypeOfIn(expr string, schemas map[string]string, ctxSchema ContextSchema, roots ...string) (cue.Kind, error) {
	if err := ValidateRoots(expr, roots...); err != nil {
		return cue.BottomKind, err
	}

	refs, err := References(expr)
	if err != nil {
		return cue.BottomKind, err
	}

	// A read out of a `_` field has no declared type, so the author has to supply
	// one. That is the same shape as the rule for a value that may be absent:
	// where the schema stops saying something, the usage has to.
	//
	// Without this the read types as unknown, admission declines to judge it, and
	// a struct landing in a string parameter is only caught if the source happens
	// to resolve during the dry-run render.
	asserted, err := Assertions(expr)
	if err != nil {
		return cue.BottomKind, err
	}

	ctx := newContext()
	sources, err := sentinelSources(ctx, schemas, refs)
	if err != nil {
		return cue.BottomKind, err
	}

	for _, ref := range refs {
		if !PathIsOpen(ref, schemas) {
			continue
		}
		typeName, ok := asserted[ref.String()]
		if !ok {
			return cue.BottomKind, fmt.Errorf("%s reads a field the source's schema leaves open, so its "+
				"type has to be given here: write %s & <type>, one of %s",
				ref, ref, strings.Join(AssertedKinds(), ", "))
		}
		materialiseAsserted(sources[ref.Path[0]], ref.Path[1:], typeName)
	}
	contextFields, err := sentinelContext(refs, ctxSchema)
	if err != nil {
		return cue.BottomKind, err
	}

	out, err := evalIn(ctx, buildScope(ctx, sources, contextFields), expr)
	if err != nil {
		return cue.BottomKind, err
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
