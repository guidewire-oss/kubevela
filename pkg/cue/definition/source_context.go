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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kubevela/workflow/pkg/cue/process"

	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"
	"github.com/oam-dev/kubevela/pkg/definition/cachekey"
)

// sourceContextFile renders the `context:` a source template is compiled against.
//
// A source does not get the component's context. It gets exactly the fields the
// cache-key rules make readable, which is what keeps inference and the runtime
// from disagreeing: a field that would not contribute to the key is not present
// to be read. Admission rejects a template that reaches for one, and this makes
// the rejection unnecessary rather than merely correct - a definition applied
// with the webhook disabled still cannot depend on something the key ignores.
//
// context.name is the binding - the spec.sources[] entry this resolution is for -
// not the consuming component. That is the sense context.name carries everywhere
// else in KubeVela: the instance being rendered, rather than the definition it
// instantiates.
func sourceContextFile(ctx process.Context, bindingName string, fields []string) (string, error) {
	data := map[string]interface{}{}
	for _, field := range fields {
		if field == velaprocess.ContextName {
			continue // supplied below, from the binding rather than the component
		}
		if v := ctx.GetData(field); v != nil {
			data[field] = v
		}
	}
	data[velaprocess.ContextName] = bindingName

	raw, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("rendering source context: %w", err)
	}
	return fmt.Sprintf("context: %s", string(raw)), nil
}

// sourceContext renders the context for a binding using the current rules.
func sourceContext(ctx process.Context, bindingName string) (string, error) {
	rules, err := cachekey.LoadRules()
	if err != nil {
		return "", err
	}
	return sourceContextFile(ctx, bindingName, rules.Fields())
}

// identityContext gathers the values named by the generated keyInputs list.
//
// Exactly those, and no more: hashing every label on the object would change the
// identity whenever GitOps stamped an unrelated annotation, leaving the cache
// permanently cold. A field that is absent contributes nil and one that is
// present but empty contributes "", because a template may branch on the
// difference and the identity has to draw it too.
func identityContext(ctx process.Context, bindingName string, inputs []string) map[string]interface{} {
	if len(inputs) == 0 {
		return nil
	}
	out := map[string]interface{}{}
	for _, in := range inputs {
		field, index, indexed := splitIndexed(in)

		var value interface{}
		switch {
		case field == velaprocess.ContextName:
			value = bindingName
		case indexed:
			value = lookupIndex(ctx.GetData(field), index)
		default:
			value = ctx.GetData(field)
		}

		if indexed {
			nested, _ := out[field].(map[string]interface{})
			if nested == nil {
				nested = map[string]interface{}{}
				out[field] = nested
			}
			nested[index] = value
			continue
		}
		out[field] = value
	}
	return out
}

// splitIndexed parses "appLabels[team]" into its field and index.
func splitIndexed(input string) (field, index string, indexed bool) {
	open := strings.Index(input, "[")
	if open < 0 || !strings.HasSuffix(input, "]") {
		return input, "", false
	}
	return input[:open], input[open+1 : len(input)-1], true
}

// lookupIndex reads one entry from a context map, returning nil when it is
// absent - which is distinct from an entry present with an empty value.
func lookupIndex(container interface{}, index string) interface{} {
	switch m := container.(type) {
	case map[string]string:
		if v, ok := m[index]; ok {
			return v
		}
	case map[string]interface{}:
		if v, ok := m[index]; ok {
			return v
		}
	}
	return nil
}
