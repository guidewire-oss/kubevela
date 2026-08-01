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

// UndefendedReads returns the reads that could be absent at render and carry no
// default.
//
// A default is not needed for most reads. A source's `schema:` is a contract the
// resolver enforces - output is validated against it before anything is cached -
// so a *required* schema field is guaranteed present, and demanding a fallback
// for it would be noise. Only an *optional* field, or a context value that has no
// schema at all, can go missing.
//
// This mirrors what fromSource already does, where a default is mandatory exactly
// when an optional source field feeds a required parameter. The decision is
// target-aware and the target is not this package's business, so the undefended
// reads are returned rather than judged: admission pairs them with the parameter
// it is filling and errors only when that parameter is required.
func UndefendedReads(raw string, schemas map[string]string) ([]Reference, error) {
	parsed, err := Parse(raw)
	if err != nil {
		return nil, err
	}

	ctx := newContext()
	compiled := map[string]cue.Value{}
	for name, schema := range schemas {
		v := ctx.CompileString(schema)
		if v.Err() != nil {
			return nil, fmt.Errorf("source %q has an unreadable schema: %w", name, v.Err())
		}
		compiled[name] = v
	}

	var out []Reference
	for _, f := range parsed.Fragments {
		if !f.IsExpr() {
			continue
		}
		refs, err := References(f.Expr)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if ref.Defaulted {
				continue
			}
			mayBeAbsent, err := canBeAbsent(ref, compiled)
			if err != nil {
				return nil, err
			}
			if mayBeAbsent {
				out = append(out, ref)
			}
		}
	}
	return out, nil
}

// canBeAbsent reports whether a read might find nothing at render.
func canBeAbsent(ref Reference, schemas map[string]cue.Value) (bool, error) {
	if !ref.IsSource() {
		// Context has no schema. An indexed read - a label or annotation - is a
		// lookup into an open map and may find nothing; a plain field is always
		// supplied, even when its value is empty.
		return len(ref.Path) > 1, nil
	}

	binding := ref.Path[0]
	schema, ok := schemas[binding]
	if !ok {
		// Nothing to judge against. The read is checked elsewhere - TypeOf
		// reports an unknown source - so this is not the place to complain.
		return false, nil
	}
	return optionalPath(schema, ref.Path[1:])
}

// optionalPath reports whether any segment of a path may be absent at render.
//
// Any segment, not just the last: if `network?: {vpcId: string}` then
// network.vpcId is absent whenever network is, however required vpcId looks
// inside it.
//
// A key read out of an open map counts too. `labels: [string]: string` declares
// the map, never a key, so `labels["team"]` may find nothing for exactly the same
// reason `context.appLabels["team"]` may - and needs a default for the same
// reason.
func optionalPath(v cue.Value, path []string) (bool, error) {
	cur := v
	for _, segment := range path {
		if pattern := cur.LookupPath(cue.MakePath(cue.AnyString)); pattern.Exists() {
			if !cur.LookupPath(cue.MakePath(cue.Str(segment))).Exists() {
				return true, nil
			}
		}
		optional, next, found, err := fieldByName(cur, segment)
		if err != nil {
			return false, err
		}
		if !found {
			// An undeclared field. TypeOf reports it with a better message than
			// anything this function could produce.
			return false, nil
		}
		if optional {
			return true, nil
		}
		cur = next
	}
	return false, nil
}

func fieldByName(v cue.Value, name string) (optional bool, value cue.Value, found bool, err error) {
	iter, ierr := v.Fields(cue.Optional(true))
	if ierr != nil {
		return false, cue.Value{}, false, ierr
	}
	for iter.Next() {
		if iter.Selector().Unquoted() == name {
			return iter.IsOptional(), iter.Value(), true, nil
		}
	}
	return false, cue.Value{}, false, nil
}

// PathIsOpen reports whether a source read descends into a `_` field.
//
// A schema may declare a field without declaring its shape - `content: _` is how
// a generic source says "whatever this file happens to be". Nothing can be typed
// through that, so the honest answer is "unknown" rather than a guess: TypeOf
// returns BottomKind, and kindsCompatible already treats unknown on either side
// as "do not block".
func PathIsOpen(ref Reference, schemas map[string]string) bool {
	if !ref.IsSource() || len(ref.Path) < 2 {
		return false
	}
	schema, ok := schemas[ref.Path[0]]
	if !ok {
		return false
	}
	v := newContext().CompileString(schema)
	if v.Err() != nil {
		return false
	}

	cur := v
	for _, segment := range ref.Path[1:] {
		if cur.IncompleteKind() == cue.TopKind {
			// Already inside an open region; everything below it is open too.
			return true
		}
		_, next, found, err := fieldByName(cur, segment)
		if err != nil || !found {
			return false
		}
		cur = next
	}
	return cur.IncompleteKind() == cue.TopKind
}
