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

package propexpr

import (
	"strconv"

	"cuelang.org/go/cue"
)

// UndefendedIn returns the reads that could be absent at render and carry no
// guard, given the reads an expression makes.
//
// It takes references rather than raw text so this package stays free of the
// expression language: a source's `schema:` is CUE and always will be, so
// deciding whether a path may be absent is schema analysis - but *which* paths an
// expression reads is a question for whichever language the expression is in.
//
// A default is not needed for most reads. A schema is a contract, so a declared
// non-optional field is always present and demanding a fallback for it would be
// noise. It is needed exactly where the schema does not promise the value:
// an optional field, or a key of an open map.
func UndefendedIn(refs []Reference, schemas map[string]string) ([]Reference, error) {
	compiled := map[string]cue.Value{}
	for name, text := range schemas {
		v := newContext().CompileString(text)
		if v.Err() != nil {
			continue
		}
		compiled[name] = v
	}

	var out []Reference
	for _, ref := range refs {
		// Both roots, not just source. An unguarded read of an absent label -
		// context.appLabels["team"] - fails the render with "no such key" exactly
		// as an absent source field does, so admission owes the author the same
		// warning. canBeAbsent judges each root by what it knows.
		if ref.Defaulted {
			continue
		}
		absent, err := canBeAbsent(ref, compiled)
		if err != nil {
			return nil, err
		}
		if absent {
			out = append(out, ref)
		}
	}
	return out, nil
}

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
		if cur.IncompleteKind() == cue.TopKind {
			// Inside a `_` field. Whether this key exists is unknowable here, so
			// no default is demanded - the same call TypeOf makes. Requiring one
			// for every read from a generic source would be noise, and would not
			// be a judgement admission is entitled to make.
			//
			// The cost is that an absent key surfaces at render rather than at
			// admission. That is the trade a source makes by declaring `_`.
			return false, nil
		}
		if cur.IncompleteKind() == cue.ListKind {
			// A list index. `outputs: [...{kind: string}]` declares what an element
			// is and says nothing about how many there are, so whether this index
			// exists is not knowable at admission - the same position as a key read
			// out of an open map, and it earns a default for the same reason.
			//
			// A schema that *does* pin the position - `[string, ...]` - answers the
			// question, and such a read needs no default.
			index, err := strconv.Atoi(segment)
			if err != nil {
				//nolint:nilerr // a non-numeric segment is not an index, not a failure
				return false, nil
			}
			if pinned := cur.LookupPath(cue.MakePath(cue.Index(index))); pinned.Exists() {
				cur = pinned
				continue
			}
			return true, nil
		}
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
