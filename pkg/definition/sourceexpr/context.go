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

// sentinelContext builds the context sentinels for exactly the fields an
// expression references.
//
// Only the referenced fields, because an open map cannot be typed wholesale: a
// CUE pattern constraint - appLabels: [string]: "x" - does not make
// context.appLabels["anything"] resolve, it still reports an undefined field. So
// the keys actually read are materialised individually, which References()
// already knows.
func sentinelContext(refs []Reference) (map[string]interface{}, error) {
	out := map[string]interface{}{}

	for _, ref := range refs {
		if ref.IsSource() {
			continue
		}
		field := ref.Path[0]
		kind, ok := contextTypes[field]
		if !ok {
			return nil, fmt.Errorf("context.%s is not a supported value in a property expression; "+
				"readable fields are %s", field, strings.Join(readableContextFields(), ", "))
		}

		switch kind {
		case kindIndexedString:
			if len(ref.Path) < 2 {
				return nil, fmt.Errorf("context.%s must be read with a key, e.g. context.%s[\"my-label\"]",
					field, field)
			}
			nested, _ := out[field].(map[string]interface{})
			if nested == nil {
				nested = map[string]interface{}{}
				out[field] = nested
			}
			nested[ref.Path[1]] = "x"
		case kindString:
			out[field] = "x"
		case kindInt:
			out[field] = 1
		case kindStruct:
			// clusterVersion. Its shape is fixed, so the sentinel can be too.
			out[field] = map[string]interface{}{
				"major": "1", "minor": "31", "gitVersion": "x", "platform": "x",
			}
		}
	}
	return out, nil
}

// contextValues selects the real values for the referenced fields, for render.
//
// A referenced key that is absent is left absent rather than defaulted. CUE then
// reports an undefined field - unless the read carries a default, which is the
// supported way to survive it.
func contextValues(refs []Reference, values map[string]interface{}) (map[string]interface{}, error) {
	out := map[string]interface{}{}

	for _, ref := range refs {
		if ref.IsSource() {
			continue
		}
		field := ref.Path[0]
		if _, ok := contextTypes[field]; !ok {
			return nil, fmt.Errorf("context.%s is not a supported value in a property expression", field)
		}
		if value, ok := values[field]; ok {
			out[field] = value
		}
	}
	return out, nil
}

func readableContextFields() []string {
	out := make([]string, 0, len(contextTypes))
	for name := range contextTypes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
