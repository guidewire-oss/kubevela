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

package application

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/definition/sourceexpr"
)

// validateExpressions checks the $(...) expressions in an Application's
// properties before it is admitted.
//
// Only what is decidable without values: the expression parses, uses no
// construct whose result type would depend on data admission has not seen, and
// reaches no identifier this surface does not offer. A mistake then surfaces
// against the property that contains it, rather than as a CUE error during a
// render that has already been accepted.
//
// Roots vary by surface. An Application-scoped policy renders before the appfile
// exists, so it has no sources to read - permitting context there and refusing
// source is what lets the surface carry expressions at all.
func validateExpressions(app *v1beta1.Application) field.ErrorList {
	var errs field.ErrorList

	check := func(raw *runtime.RawExtension, path *field.Path, roots ...string) {
		if raw == nil || len(raw.Raw) == 0 {
			return
		}
		var decoded interface{}
		if err := json.Unmarshal(raw.Raw, &decoded); err != nil {
			// Malformed properties are reported by the consumer's own parsing.
			return
		}
		if !sourceexpr.HasExpression(decoded) {
			return
		}
		if err := sourceexpr.ValidateTree(decoded, roots...); err != nil {
			errs = append(errs, field.Invalid(path, string(raw.Raw), err.Error()))
		}
	}

	both := []string{sourceexpr.SourceIdent, sourceexpr.ContextIdent}
	contextOnly := []string{sourceexpr.ContextIdent}

	for i, comp := range app.Spec.Components {
		p := field.NewPath("spec", "components").Index(i)
		check(comp.Properties, p.Child("properties"), both...)
		for j, tr := range comp.Traits {
			check(tr.Properties, p.Child("traits").Index(j).Child("properties"), both...)
		}
	}
	for i, src := range app.Spec.Sources {
		check(src.Properties, field.NewPath("spec", "sources").Index(i).Child("properties"), both...)
	}
	for i, policy := range app.Spec.Policies {
		check(policy.Properties, field.NewPath("spec", "policies").Index(i).Child("properties"), contextOnly...)
	}
	if app.Spec.Workflow != nil {
		for i, step := range app.Spec.Workflow.Steps {
			p := field.NewPath("spec", "workflow", "steps").Index(i)
			check(step.Properties, p.Child("properties"), both...)
			for j, sub := range step.SubSteps {
				check(sub.Properties, p.Child("subSteps").Index(j).Child("properties"), both...)
			}
		}
	}

	return errs
}
