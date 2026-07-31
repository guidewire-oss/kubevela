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
