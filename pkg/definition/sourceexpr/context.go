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

	"cuelang.org/go/cue"
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

// ContextSchema is the readable context for one surface: which fields exist,
// with what type, and why the others do not.
//
// It is a view over the registry in context.cue, not a declaration of its own.
// The types are the CUE types written there, so what admission checks and what
// render supplies come from the same place.
type ContextSchema struct {
	// Surface names the schema in error messages.
	Surface string
	// key is the registry's own name for this surface.
	key string
	// value is the composed type for this surface. A zero value means an unknown
	// surface, which reports every field as unavailable.
	value cue.Value
	// excluded explains each field the render context carries that no surface
	// offers, so a new one has to be classified rather than silently ignored.
	excluded map[string]string
}

// ComponentContext is what a ComponentDefinition or TraitDefinition sees.
//
// Membership follows one rule: an expression sees what the definition it is
// feeding sees, at the moment it is rendered. A property expression is
// substituted immediately before the template runs, so the readable set is that
// template's context - not the cache-key rules, which are policy about a
// SourceDefinition's cache identity and curate a different set for a different
// purpose.
//
// That rule also settles context.name. In a SourceDefinition it is the binding
// entry (KEP amendment A4); here it is the component, because that is what a
// ComponentDefinition's context.name is.
var ComponentContext = surfaceSchema("component")

// TraitContext is what a TraitDefinition sees: its component's identity, plus
// its own type. A trait has no instance name in the API to expose alongside it.
var TraitContext = surfaceSchema("trait")

// WorkflowStepContext is what a workflow step's properties see. The step's
// properties are substituted before the engine receives them, from a context
// built the same way a component's is.
var WorkflowStepContext = surfaceSchema("workflowstep")

// PolicyContext is what a resource-rendering policy sees.
//
// Narrower than ScopedPolicyContext because the two policy paths run at
// different times against different data: this one substitutes while the appfile
// is built, before any render, from what the Appfile carries. There is no
// cluster yet and no policy revision metadata.
var PolicyContext = surfaceSchema("policy-default")

// ScopedPolicyContext is what an Application-scoped PolicyDefinition sees.
//
// It gets revision metadata and clusterVersion but no cluster: that render
// targets no cluster at all. context.name is omitted on both policy surfaces
// because it means the Application on one path and the policy on the other -
// expressions read appName or policyName, which say what they are.
var ScopedPolicyContext = surfaceSchema("policy-app")

// field returns the declared type of a context field on this surface.
func (c ContextSchema) field(name string) (cue.Value, bool) {
	if !c.value.Exists() {
		return cue.Value{}, false
	}
	v := c.value.LookupPath(cue.MakePath(cue.Str(name)))
	return v, v.Exists()
}

// pathIsOpen reports whether a context read descends into a field the registry
// leaves unshaped.
//
// `custom: _` is the case this exists for: an Application-scoped policy chooses
// its shape, so nothing below it can be typed and the read has to say what it
// expects. Mirrors PathIsOpen on the source side.
func (c ContextSchema) pathIsOpen(path []string) bool {
	if len(path) < 2 {
		// The field itself, not a read through it. A whole `_` value types as
		// whatever it holds and needs no assertion.
		return false
	}
	cur, ok := c.field(path[0])
	if !ok {
		return false
	}
	for _, segment := range path[1:] {
		if cur.IncompleteKind() == cue.TopKind {
			return true
		}
		next := cur.LookupPath(cue.MakePath(cue.Str(segment)))
		if !next.Exists() {
			return false
		}
		cur = next
	}
	return cur.IncompleteKind() == cue.TopKind
}

// Offers reports whether this surface makes a context field readable.
func (c ContextSchema) Offers(field string) bool {
	_, ok := c.field(field)
	return ok
}

// isIndexed reports a field that is an open map - appLabels and friends - which
// must be read with a key.
func (c ContextSchema) isIndexed(name string) bool {
	v, ok := c.field(name)
	if !ok {
		return false
	}
	return v.LookupPath(cue.MakePath(cue.AnyString)).Exists()
}

// sentinelContext builds the context sentinels for exactly the fields an
// expression references.
//
// Only the referenced fields, because an open map cannot be typed wholesale: a
// CUE pattern constraint - appLabels: [string]: "x" - does not make
// context.appLabels["anything"] resolve, it still reports an undefined field. So
// the keys actually read are materialised individually, which References()
// already knows.
func sentinelContext(refs []Reference, schema ContextSchema) (map[string]interface{}, error) {
	out := map[string]interface{}{}

	for _, ref := range refs {
		if ref.IsSource() {
			continue
		}
		field := ref.Path[0]
		declared, ok := schema.field(field)
		if !ok {
			return nil, fmt.Errorf("context.%s is not readable in %s properties%s; readable fields are %s",
				field, schema.Surface, schema.why(field), strings.Join(schema.readable(), ", "))
		}

		// An open map has no concrete field at any key, so the key actually read
		// is materialised from the pattern's type.
		if schema.isIndexed(field) {
			if len(ref.Path) < 2 {
				return nil, fmt.Errorf("context.%s must be read with a key, e.g. context.%s[\"my-label\"]",
					field, field)
			}
			value, err := sentinelFor(declared.LookupPath(cue.MakePath(cue.AnyString)))
			if err != nil {
				return nil, fmt.Errorf("context.%s: %w", field, err)
			}
			nested, _ := out[field].(map[string]interface{})
			if nested == nil {
				nested = map[string]interface{}{}
				out[field] = nested
			}
			nested[ref.Path[1]] = value
			continue
		}

		// Everything else comes from the declared type, through the same builder
		// the source schemas use - so clusterVersion.minor is an int here because
		// the registry says it is, not because a Go switch remembered to say so.
		value, err := sentinelFor(declared)
		if err != nil {
			return nil, fmt.Errorf("context.%s: %w", field, err)
		}
		out[field] = value
	}
	return out, nil
}

// contextValues selects the real values for the referenced fields, for render.
//
// A referenced key that is absent is left absent rather than defaulted. CUE then
// reports an undefined field - unless the read carries a default, which is the
// supported way to survive it.
func contextValues(refs []Reference, values map[string]interface{}, schema ContextSchema) (map[string]interface{}, error) {
	out := map[string]interface{}{}

	for _, ref := range refs {
		if ref.IsSource() {
			continue
		}
		field := ref.Path[0]
		if _, ok := schema.field(field); !ok {
			return nil, fmt.Errorf("context.%s is not readable in %s properties", field, schema.Surface)
		}
		if value, ok := values[field]; ok {
			out[field] = value
		}
	}
	return out, nil
}

// readable lists the fields this surface exposes, for an error message.
func (c ContextSchema) readable() []string {
	if !c.value.Exists() {
		return nil
	}
	iter, err := c.value.Fields()
	if err != nil {
		return nil
	}
	var out []string
	for iter.Next() {
		out = append(out, iter.Selector().Unquoted())
	}
	sort.Strings(out)
	return out
}

// why appends the recorded reason for an excluded field, so the author is told
// that it exists and why they cannot have it rather than that it is unknown.
func (c ContextSchema) why(field string) string {
	if reason, ok := c.excluded[field]; ok {
		return " (" + reason + ")"
	}
	// Available somewhere, just not here. Saying where beats saying nothing, and
	// beats prose that has to be kept true by hand.
	if others := elsewhere(field, c.key); len(others) > 0 {
		return " (available on: " + strings.Join(others, ", ") + ")"
	}
	return ""
}
