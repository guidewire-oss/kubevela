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
)

// ContextIdent is the second identifier an expression may reference, so an
// author can wire a render-context value straight into a parameter:
//
//	properties:
//	  cluster: '$(context.appLabels["cluster-name"])'
//
// KEP-2.16 lists this under Non-Goals: "OAM context fields needed in properties
// should be exposed via a SourceDefinition authored by the platform engineer,
// keeping the resolution model consistent". That rationale is worth re-examining
// rather than assuming, which is what this spike is for - see the devlog.
//
// The important structural difference from a SourceDefinition's context: a
// property expression feeds no cache. A source's context reads are restricted
// because they determine cache identity, and reading an unkeyed value would
// break sharing. Nothing is shared here - the expression is evaluated per render
// - so that constraint simply does not apply.
const ContextIdent = "context"

// contextKind is how a context field is represented, which is all the type
// checker needs to know about it.
type contextKind int

const (
	kindString contextKind = iota
	kindInt
	// kindIndexedString is an open map of string to string - appLabels and
	// appAnnotations. Any key may be read, so the sentinel for one cannot be
	// written ahead of time; it is materialised per referenced key.
	kindIndexedString
	// kindStruct is a value with no single scalar form, readable only through
	// its fields.
	kindStruct
)

// contextTypes declares the type of each readable context field.
//
// Membership is not decided here - it comes from the cache-key rules, and
// TestContextTypesMatchTheKeyRules asserts the two agree so they cannot drift.
// Reusing that list keeps one curated set of "context a consumer may read",
// already excludes the fields that must never be readable (context.output,
// context.status), and gives the same error message for anything outside it.
//
// The types are declared here because the rules file is policy about the *cache
// key* and has no business carrying type information.
var contextTypes = map[string]contextKind{
	"name":           kindString,
	"cluster":        kindString,
	"clusterVersion": kindStruct,
	"namespace":      kindString,
	"appName":        kindString,
	"appRevision":    kindString,
	"appRevisionNum": kindInt,
	"publishVersion": kindString,
	"workflowName":   kindString,
	"appLabels":      kindIndexedString,
	"appAnnotations": kindIndexedString,
}

// contextScope renders `context: {...}` containing sentinels for exactly the
// fields an expression references.
//
// Only the referenced fields, because an open map cannot be typed wholesale: a
// CUE pattern constraint - appLabels: [string]: "x" - does not make
// context.appLabels["anything"] resolve, it still reports an undefined field. So
// the keys actually read are materialised individually, which References()
// already knows.
func contextScope(refs []Reference) (string, error) {
	fields := map[string]string{}
	indexed := map[string]map[string]string{}

	for _, ref := range refs {
		if ref.IsSource() {
			continue
		}
		field := ref.Path[0]
		kind, ok := contextTypes[field]
		if !ok {
			return "", fmt.Errorf("context.%s is not a supported value in a property expression; "+
				"readable fields are %s", field, strings.Join(readableContextFields(), ", "))
		}

		switch kind {
		case kindIndexedString:
			if len(ref.Path) < 2 {
				return "", fmt.Errorf("context.%s must be read with a key, e.g. context.%s[\"my-label\"]",
					field, field)
			}
			if indexed[field] == nil {
				indexed[field] = map[string]string{}
			}
			indexed[field][ref.Path[1]] = `"x"`
		case kindString:
			fields[field] = `"x"`
		case kindInt:
			fields[field] = "1"
		case kindStruct:
			// clusterVersion. Its shape is fixed, so the sentinel can be too.
			fields[field] = `{major: "1", minor: "31", gitVersion: "x", platform: "x"}`
		}
	}

	var b strings.Builder
	b.WriteString(ContextIdent + ": {\n")
	for _, name := range sortedKeys(fields) {
		fmt.Fprintf(&b, "  %q: %s\n", name, fields[name])
	}
	for _, name := range sortedKeysNested(indexed) {
		fmt.Fprintf(&b, "  %q: {", name)
		first := true
		for _, key := range sortedKeys(indexed[name]) {
			if !first {
				b.WriteString(", ")
			}
			first = false
			fmt.Fprintf(&b, "%q: %s", key, indexed[name][key])
		}
		b.WriteString("}\n")
	}
	b.WriteString("}\n")
	return b.String(), nil
}

// contextValueScope renders the same shape with real values, for render time.
//
// A referenced key that is absent is left absent rather than defaulted. CUE then
// reports an undefined field, which is the honest outcome: admission cannot know
// whether a label exists, and silently substituting "" would let a missing label
// flow into a parameter as an empty string. See the devlog - defaulting needs a
// syntax this spike does not have, since conditionals are barred by the grammar
// gate and `|` is a disjunction rather than a fallback.
func contextValueScope(refs []Reference, values map[string]interface{}) (string, error) {
	present := map[string]interface{}{}

	for _, ref := range refs {
		if ref.IsSource() {
			continue
		}
		field := ref.Path[0]
		if _, ok := contextTypes[field]; !ok {
			return "", fmt.Errorf("context.%s is not a supported value in a property expression", field)
		}
		value, ok := values[field]
		if !ok {
			continue
		}
		present[field] = value
	}

	raw, err := marshalScope(present)
	if err != nil {
		return "", err
	}
	return ContextIdent + ": " + raw, nil
}

func readableContextFields() []string {
	out := make([]string, 0, len(contextTypes))
	for name := range contextTypes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysNested(m map[string]map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
