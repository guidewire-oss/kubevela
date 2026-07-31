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
	"fmt"

	"github.com/kubevela/workflow/pkg/cue/process"
)

// Surfaces an Application can carry a fromSource directive on.
//
// These live here, next to the resolver, because which of them actually resolve
// is a property of this package: substitution is wired into workloadDef.Complete
// and traitDef.Complete and nowhere else. The admission webhooks and the appfile
// parser both enforce that rule, so it is stated once rather than restated per
// enforcement point.
const (
	// SurfaceComponent is a directive in a component's properties.
	SurfaceComponent = "component"
	// SurfaceTrait is a directive in a trait's properties.
	SurfaceTrait = "trait"
	// SurfacePolicy is a directive in a policy's properties.
	SurfacePolicy = "policy"
	// SurfaceWorkflowStep is a directive in a workflow step's (or sub-step's) properties.
	SurfaceWorkflowStep = "workflowstep"
	// SurfaceSource is a directive in a later spec.sources[] entry's properties,
	// i.e. source chaining. It resolves because the chain is walked during a
	// component render.
	SurfaceSource = "source"
)

// ConsumableSurfaces are the surfaces a SourceDefinition may name in
// consumableFrom: the places an Application actually consumes a resolved value.
// Source chaining is excluded - it is plumbing between sources, not consumption.
var ConsumableSurfaces = []string{SurfaceComponent, SurfaceTrait}

// SurfaceResolvesFromSource reports whether a fromSource directive on this
// surface is substituted at reconcile time.
//
// Anywhere else the directive is inert: it survives into the consumer as a
// literal {"fromSource": ...} map, which either fails CUE type-checking with a
// confusing message or, where the target parameter is open, is silently accepted
// as a value. Both enforcement points reject it instead.
func SurfaceResolvesFromSource(surface string) bool {
	switch surface {
	case SurfaceComponent, SurfaceTrait, SurfaceSource, SurfaceWorkflowStep:
		return true
	default:
		return false
	}
}

// UnsupportedSurfaceMessage is the single wording used wherever a directive on
// an unresolvable surface is rejected, so admission and the parser report the
// same thing.
func UnsupportedSurfaceMessage(surface string) string {
	return fmt.Sprintf("fromSource is not supported in %s properties; it is resolved during component and trait rendering only", surface)
}

// HasFromSourceDirective reports whether decoded properties contain a fromSource
// directive at any depth.
func HasFromSourceDirective(node interface{}) bool {
	switch v := node.(type) {
	case map[string]interface{}:
		if _, ok := v["fromSource"]; ok {
			return true
		}
		for _, child := range v {
			if HasFromSourceDirective(child) {
				return true
			}
		}
	case []interface{}:
		for _, child := range v {
			if HasFromSourceDirective(child) {
				return true
			}
		}
	}
	return false
}

// ResolveFromSourceParams substitutes fromSource directives in a properties
// blob, for callers outside the component and trait render paths.
//
// EXPERIMENT: added to test whether workflow steps can be supported by
// substituting before the workflow engine sees them, rather than by changing
// that engine. See devlogs/2026-07-31-source-expressions.md.
func ResolveFromSourceParams(ctx process.Context, params interface{}) (interface{}, error) {
	return resolveFromSourceParams(ctx, params)
}
