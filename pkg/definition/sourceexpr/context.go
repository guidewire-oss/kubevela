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
// Membership follows one rule: an expression sees what the definition it is
// feeding sees, at the moment it is rendered. A property expression is
// substituted immediately before the ComponentDefinition's template runs, so the
// readable set is that template's context - not the cache-key rules, which are
// policy about a SourceDefinition's cache identity and curate a different set
// for a different purpose.
//
// That rule also settles context.name. In a SourceDefinition it is the binding
// entry (KEP amendment A4); here it is the component, because that is what a
// ComponentDefinition's context.name is. Each scope is internally consistent
// with the definition it belongs to, which is the only property that can be kept
// as more surfaces are added.
//
// TestContextTypesMatchTheRenderContext builds a real render context and
// requires every field in it to be classified here or in notReadable, so a field
// added upstream forces a decision instead of silently becoming unavailable.
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
	"replicaKey":     kindString,
	"revision":       kindString,
	"appLabels":      kindIndexedString,
	"appAnnotations": kindIndexedString,
}

// notReadable are context fields a property expression deliberately cannot see,
// each with the reason. Being explicit is what makes the drift test useful: a new
// field cannot be quietly ignored, it has to be put in one list or the other.
var notReadable = map[string]string{
	"appSources":               "internal plumbing for source resolution, not user-facing context",
	"appSourceTypes":           "internal plumbing for source resolution, not user-facing context",
	"appSourceTemplates":       "internal plumbing for source resolution, not user-facing context",
	"appSourceSensitivePaths":  "internal plumbing for source resolution, not user-facing context",
	"appSourceCacheStore":      "internal plumbing for source resolution, not user-facing context",
	"sourceResolutionStatuses": "internal plumbing for source resolution, not user-facing context",
	"components":               "an app-wide list; readable in principle but not yet typed here",
	"appComponents":            "an app-wide list; readable in principle but not yet typed here",
	"appPolicies":              "an app-wide list; readable in principle but not yet typed here",
	"appWorkflow":              "an app-wide object; readable in principle but not yet typed here",
	"output":                   "produced by the render, so it does not exist when properties are substituted",
	"outputs":                  "produced by the render, so it does not exist when properties are substituted",
	"outputSecretName":         "produced by the render, so it does not exist when properties are substituted",
	"parameter":                "the properties being substituted; reading them from within is circular",
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
			// clusterVersion. Its shape is fixed, so the sentinel can be too -
			// but it has to match what parseClusterVersion actually builds.
			// minor is an int64 there (strconv.ParseInt), not a string, and
			// declaring it a string made admission promise a type render would
			// not produce.
			out[field] = map[string]interface{}{
				"major":      "1",
				"minor":      1,
				"gitVersion": "x",
				"platform":   "x",
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
