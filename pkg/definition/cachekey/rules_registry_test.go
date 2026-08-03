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

package cachekey

import (
	"reflect"
	"strings"
	"testing"

	"github.com/oam-dev/kubevela/pkg/definition/sourceexpr"
)

// sourceResolvingSurfaces are the call sites where a SourceDefinition's template
// is actually executed.
//
// Named here rather than imported from pkg/cue/definition, which imports this
// package - the list is short, and stating it keeps the dependency one-way.
// Source chaining is absent deliberately: a chained source resolves inside
// whichever render triggered the outer binding, so it introduces no context of
// its own.
var sourceResolvingSurfaces = []string{"component", "trait", "workflowstep"}

// A keyed field must exist in the context registry, and must exist on every
// surface where a source can resolve.
//
// The rules file is hand-edited and its hash is stamped onto every generated
// definition, so a mistake there is expensive and quiet. Two ways to get it
// wrong:
//
//   - a typo, or a field that simply does not exist. sourceContext builds a
//     source's context from this list, so the field is silently absent and the
//     template fails at render with an undefined field.
//   - a field that exists but not everywhere. A source reading it would resolve
//     from a component and fail from a workflow step, depending on where an
//     Application happened to consume it.
//
// Every rules file is checked, not just the current one, so a version added
// later is covered without remembering to extend this.
func TestKeyedFieldsExistInTheContextRegistry(t *testing.T) {
	names, err := ruleFileNames()
	if err != nil {
		t.Fatalf("listing rule files: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no rules files are embedded")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			rules, err := loadRuleFile(name)
			if err != nil {
				t.Fatalf("loading: %v", err)
			}
			fields := rules.Fields()
			if len(fields) == 0 {
				t.Fatal("declares no keyed fields")
			}

			for _, field := range fields {
				var missing []string
				for _, surface := range sourceResolvingSurfaces {
					if !sourceexpr.SurfaceOffers(surface, field) {
						missing = append(missing, surface)
					}
				}
				switch {
				case len(missing) == len(sourceResolvingSurfaces):
					t.Errorf("keyed field %q is not in the context registry at all; "+
						"a source reading it would find nothing at render. Add it to a group "+
						"in pkg/definition/sourceexpr/context.cue, or remove it here", field)
				case len(missing) > 0:
					t.Errorf("keyed field %q is not offered on %v, so a source reading it would "+
						"resolve from some call sites and fail at others. A field may only be "+
						"keyed if every surface that resolves sources offers it", field, missing)
				}
			}
		})
	}
}

// The registry must describe every surface the rules are checked against, or the
// check above passes by looking at nothing.
func TestSourceResolvingSurfacesAreDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, name := range sourceexpr.SurfaceNames() {
		declared[name] = true
	}
	for _, surface := range sourceResolvingSurfaces {
		if !declared[surface] {
			t.Errorf("surface %q is checked here but the context registry does not declare it; "+
				"registry surfaces are %v", surface, sourceexpr.SurfaceNames())
		}
	}
}

// The surface compatibility primitives, exercised against fields the registry
// genuinely treats differently.
//
// The check cannot be reached through a real SourceDefinition yet: the rules
// permit only universally-available fields, so a template reading a
// surface-specific one is refused before this could run. Testing the primitives
// directly is what shows the guard will work the day that changes - which is the
// whole reason it lands first.
func TestSurfaceCompatibilityPrimitives(t *testing.T) {
	all := []string{"component", "trait", "workflowstep", "policy-default", "policy-app"}

	t.Run("a universal field resolves anywhere", func(t *testing.T) {
		for _, surface := range all {
			if err := CheckSurface([]string{"appName", "namespace"}, surface); err != nil {
				t.Errorf("%s: %v", surface, err)
			}
		}
	})

	t.Run("a component field narrows to component surfaces", func(t *testing.T) {
		got := SurfacesSupporting([]string{"replicaKey"}, all)
		want := []string{"component", "trait"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if err := CheckSurface([]string{"replicaKey"}, "workflowstep"); err == nil {
			t.Error("a workflow step has no replicaKey; expected a refusal")
		}
	})

	t.Run("a policy field narrows to policy surfaces", func(t *testing.T) {
		got := SurfacesSupporting([]string{"policyName"}, all)
		want := []string{"policy-app", "policy-default"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	// The case worth catching at definition time: fields that no single surface
	// can satisfy together, so the source is unusable everywhere.
	t.Run("fields from different surfaces satisfy none", func(t *testing.T) {
		if got := SurfacesSupporting([]string{"replicaKey", "policyName"}, all); len(got) != 0 {
			t.Fatalf("no surface has both; got %v", got)
		}
	})

	// context.name comes from the binding, not the caller - KEP A4 - so no
	// surface can withhold it even though none declares it.
	t.Run("context.name is always available", func(t *testing.T) {
		for _, surface := range all {
			if err := CheckSurface([]string{"name"}, surface); err != nil {
				t.Errorf("%s: name comes from the binding: %v", surface, err)
			}
		}
	})

	// Fail open, matching every other surface-aware check: a caller that has not
	// been taught to name its surface must not start failing.
	t.Run("an unknown surface accepts everything", func(t *testing.T) {
		if err := CheckSurface([]string{"replicaKey", "policyName"}, "not-a-surface"); err != nil {
			t.Errorf("expected fail-open, got %v", err)
		}
	})

	t.Run("the message names the fields and the surface", func(t *testing.T) {
		err := CheckSurface([]string{"replicaKey"}, "workflowstep")
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, want := range []string{`"replicaKey"`, "workflowstep"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message should contain %q; got %v", want, err)
			}
		}
	})
}

// Every field the current rules key on must be readable from every surface a
// source can resolve on - the same property TestKeyedFieldsExistInTheContextRegistry
// asserts, stated through the primitive the checks actually use.
func TestCurrentRulesSatisfyEverySourceSurface(t *testing.T) {
	rules, err := LoadRules()
	if err != nil {
		t.Fatal(err)
	}
	for _, surface := range sourceResolvingSurfaces {
		if err := CheckSurface(rules.Fields(), surface); err != nil {
			t.Errorf("the current rules cannot resolve on %s: %v", surface, err)
		}
	}
}
