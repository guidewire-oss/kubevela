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

package celexpr

import (
	"testing"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"

	"github.com/oam-dev/kubevela/pkg/definition/sourceexpr"
)

// The CEL environment is built from the shared context registry, so a surface
// offers the same fields to both engines. That is the property that makes a swap
// tractable at all: one declaration, two type systems reading it.
func TestSurfaceParity(t *testing.T) {
	cc := cuecontext.New()
	v := cc.CompileString("s: {host: string}").LookupPath(cue.ParsePath("s"))
	sources := map[string]cue.Value{"cfg": v}

	for _, surface := range []string{"component", "trait", "workflowstep", "policy-rendered"} {
		env, err := EnvForSurface(sources, surface)
		if err != nil {
			t.Fatalf("%s: building the env: %v", surface, err)
		}
		schema := sourceexpr.ContextFor(surface)

		// Every field the registry says this surface offers must type-check.
		for _, f := range schema.ReadableFields() {
			if f == "custom" {
				continue // `_` by construction; typed as Any, nothing to compare
			}
			if _, err := OutputType(env, "context."+f); err != nil {
				t.Errorf("%s offers context.%s but CEL rejected it: %v", surface, f, err)
			}
		}

		// And a field it does not offer must be rejected, not silently dyn.
		if _, err := OutputType(env, "context.nosuchfield"); err == nil {
			t.Errorf("%s: an undeclared context field was accepted", surface)
		}
		t.Logf("%-16s %d context fields, all typed", surface, len(schema.ReadableFields()))
	}
}

// A surface-specific field must be absent where the surface does not offer it -
// the same restriction the CUE path enforces, arrived at from the same registry.
func TestSurfaceRestrictionParity(t *testing.T) {
	cc := cuecontext.New()
	v := cc.CompileString("s: {host: string}").LookupPath(cue.ParsePath("s"))
	sources := map[string]cue.Value{"cfg": v}

	for _, tc := range []struct {
		field   string
		offered []string
		denied  []string
	}{
		{"componentName", []string{"component", "trait"}, []string{"workflowstep", "policy-rendered"}},
		{"traitType", []string{"trait"}, []string{"component", "workflowstep"}},
		{"stepName", []string{"workflowstep"}, []string{"component", "trait"}},
		{"policyName", []string{"policy-rendered"}, []string{"component", "trait", "workflowstep"}},
	} {
		for _, s := range tc.offered {
			env, err := EnvForSurface(sources, s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OutputType(env, "context."+tc.field); err != nil {
				t.Errorf("%s should offer context.%s: %v", s, tc.field, err)
			}
		}
		for _, s := range tc.denied {
			env, err := EnvForSurface(sources, s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OutputType(env, "context."+tc.field); err == nil {
				t.Errorf("%s must not offer context.%s", s, tc.field)
			}
		}
		t.Logf("context.%-14s offered on %v, denied on %v", tc.field, tc.offered, tc.denied)
	}
}

// The default form. `*read | fallback` has no CEL equivalent; has() plus a ternary
// is the replacement, so the capability survives even though the spelling changes.
func TestOptionalDefaults(t *testing.T) {
	cc := cuecontext.New()
	v := cc.CompileString(`s: {host: string, note?: string, data: [string]: string}`).
		LookupPath(cue.ParsePath("s"))
	env, err := EnvForSurface(map[string]cue.Value{"cfg": v}, "component")
	if err != nil {
		t.Fatal(err)
	}

	in := map[string]interface{}{
		"source":  map[string]interface{}{"cfg": map[string]interface{}{"host": "h", "data": map[string]interface{}{}}},
		"context": map[string]interface{}{},
	}

	for _, tc := range []struct {
		expr string
		want interface{}
	}{
		{`has(source.cfg.note) ? source.cfg.note : "none"`, "none"},
		{`source.cfg.data["absent"]`, nil}, // reading an absent map key errors
	} {
		got, err := Eval(env, tc.expr, in)
		if tc.want == nil {
			if err == nil {
				t.Errorf("%-42s should have failed, got %#v", tc.expr, got)
			} else {
				t.Logf("%-42s errors as expected: absent map key", tc.expr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%-42s ERROR %v", tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%-42s got %#v, want %#v", tc.expr, got, tc.want)
			continue
		}
		t.Logf("%-42s -> %#v", tc.expr, got)
	}
}
