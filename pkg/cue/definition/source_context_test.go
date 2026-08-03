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

package definition

import (
	"strings"
	"testing"

	"github.com/kubevela/workflow/pkg/cue/process"

	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"
	"github.com/oam-dev/kubevela/pkg/definition/cachekey"
	"github.com/oam-dev/kubevela/pkg/definition/sourceexpr"
)

// componentContext stands in for the context a component render would produce -
// including fields a source is not entitled to.
func componentContext(t *testing.T) process.Context {
	t.Helper()
	ctx := velaprocess.NewContext(velaprocess.ContextData{
		Namespace: "team-a",
		Cluster:   "prod-cluster",
		AppName:   "checkout",
		CompName:  "api",
	})
	// Pushed by PrepareProcessContext before the render, alongside the type.
	ctx.PushData(velaprocess.ContextComponentName, "api")
	ctx.PushData(velaprocess.ContextComponentType, "webservice")
	ctx.PushData(velaprocess.ContextAppLabels, map[string]string{"team": "platform"})
	return ctx
}

func TestSourceContextIsBuiltFromTheRules(t *testing.T) {
	rules, err := cachekey.LoadRules()
	if err != nil {
		t.Fatalf("loading rules: %v", err)
	}
	got, err := sourceContextFile(componentContext(t), "backstage", rules.Fields())
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}

	for _, want := range []string{`"cluster":"prod-cluster"`, `"namespace":"team-a"`, `"appName":"checkout"`} {
		if !strings.Contains(got, want) {
			t.Errorf("expected the context to carry %s; got %s", want, got)
		}
	}
}

// The point of building the context from the rules: a field that would not
// contribute to the key is absent, so it cannot be read even where admission is
// disabled. Rejecting it at admission alone would leave that gap open.
//
// Consumer identity is deliberately not in that set any more - it is keyed, so it
// reaches the template, and a source resolving per component is the reason. What
// must still be absent is everything the rules do not name: component fields left
// out of `keyed` on purpose, and internal plumbing.
func TestSourceContextOmitsUnreadableFields(t *testing.T) {
	rules, err := cachekey.LoadRules()
	if err != nil {
		t.Fatalf("loading rules: %v", err)
	}
	got, err := sourceContextFile(componentContext(t), "backstage", rules.Fields())
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}

	// In the registry and in the render context, but not keyed - caching per
	// component revision or per replica is finer than anything has needed.
	for _, unreadable := range []string{"revision", "replicaKey", "appSourceCacheStore", "artifacts"} {
		if strings.Contains(got, unreadable) {
			t.Errorf("context.%s is not keyed, so it must not reach a source template; got %s",
				unreadable, got)
		}
	}
}

// Consumer identity does reach the template, which is what makes a per-component
// source possible - its key carries the component, so each component gets its own
// cache entry.
func TestSourceContextCarriesConsumerIdentity(t *testing.T) {
	rules, err := cachekey.LoadRules()
	if err != nil {
		t.Fatalf("loading rules: %v", err)
	}
	got, err := sourceContextFile(componentContext(t), "backstage", rules.Fields())
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}

	for _, want := range []string{`"componentName":"api"`, `"componentType":"webservice"`} {
		if !strings.Contains(got, want) {
			t.Errorf("expected the source context to carry %s; got %s", want, got)
		}
	}
}

// context.name is the spec.sources[] entry, not the component consuming it.
func TestSourceContextNameIsTheBinding(t *testing.T) {
	rules, err := cachekey.LoadRules()
	if err != nil {
		t.Fatalf("loading rules: %v", err)
	}
	got, err := sourceContextFile(componentContext(t), "backstage", rules.Fields())
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}

	if !strings.Contains(got, `"name":"backstage"`) {
		t.Errorf("expected context.name to be the binding entry; got %s", got)
	}
	if strings.Contains(got, `"name":"api"`) {
		t.Errorf("context.name is the consuming component, not the binding: %s", got)
	}
}

// availableFields narrows what a source may read to what its call site offers.
//
// Now that caller identity is keyed this genuinely filters: a component render
// has no stepName, a workflow step has no componentName. The projection is what
// keeps a template from reading a field that is simply absent at that call site -
// admission refuses the binding first, but this is the backstop where admission
// is disabled.
func TestAvailableFields(t *testing.T) {
	rules, err := cachekey.LoadRules()
	if err != nil {
		t.Fatalf("loading rules: %v", err)
	}
	all := rules.Fields()

	t.Run("each surface keeps its own identity and drops the others", func(t *testing.T) {
		for _, tc := range []struct {
			surface  string
			expect   []string
			excluded []string
		}{
			{SurfaceComponent, []string{"componentName", "componentType"},
				[]string{"traitType", "stepName", "stepType", "policyName"}},
			{SurfaceTrait, []string{"componentName", "componentType", "traitType"},
				[]string{"stepName", "stepType", "policyName"}},
			{SurfaceWorkflowStep, []string{"stepName", "stepType"},
				[]string{"componentName", "componentType", "traitType", "policyName"}},
			{SurfacePolicyRendered, []string{"policyName", "policyType"},
				[]string{"componentName", "traitType", "stepName"}},
		} {
			got := availableFields(all, tc.surface)
			has := map[string]bool{}
			for _, f := range got {
				has[f] = true
			}
			for _, f := range tc.expect {
				if !has[f] {
					t.Errorf("%s should offer %s; got %v", tc.surface, f, got)
				}
			}
			for _, f := range tc.excluded {
				if has[f] {
					t.Errorf("%s has no %s, so it must not reach the template; got %v", tc.surface, f, got)
				}
			}
			// The universal fields survive everywhere, which is what keeps an
			// ordinary source consumable from any call site.
			for _, f := range []string{"name", "cluster", "namespace", "appName"} {
				if !has[f] {
					t.Errorf("%s dropped the universal field %s; got %v", tc.surface, f, got)
				}
			}
		}
	})

	t.Run("narrows to what the surface offers", func(t *testing.T) {
		// policy-app offers no cluster: that render targets none. A source is not
		// consumable there yet, but the narrowing must be real rather than
		// accidental, or the machinery proves nothing.
		got := availableFields(all, "policy-app")
		for _, field := range got {
			if field == "cluster" {
				t.Error("cluster survived narrowing to an application-scoped policy, which has none")
			}
		}
		if len(got) >= len(all) {
			t.Errorf("expected narrowing on policy-app, got %d of %d", len(got), len(all))
		}
	})

	t.Run("context.name always survives", func(t *testing.T) {
		// It is supplied from the binding, not the caller, so no surface can
		// withhold it.
		for _, surface := range append(sourceexpr.SurfaceNames(), SurfaceComponent) {
			found := false
			for _, field := range availableFields(all, surface) {
				if field == velaprocess.ContextName {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: context.name must survive; it comes from the binding", surface)
			}
		}
	})

	// Failing open matters: a caller that forgot to name its surface should
	// behave as it did before, not silently lose its whole context.
	t.Run("an unknown or empty surface fails open", func(t *testing.T) {
		for _, surface := range []string{"", "not-a-surface"} {
			if got := availableFields(all, surface); len(got) != len(all) {
				t.Errorf("%q: expected every field, got %d of %d", surface, len(got), len(all))
			}
		}
	})
}
