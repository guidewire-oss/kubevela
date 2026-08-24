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

package addon

import "fmt"

// moduleSource describes one place a module import can be fetched from.
// Only Registry is read today; OCI/Git source kinds and Version/Versions
// selection are not implemented yet (GWCP-107744 scope: registry only,
// first source only).
type moduleSource struct {
	Registry string   `json:"registry,omitempty"`
	Version  string   `json:"version,omitempty"`
	Versions []string `json:"versions,omitempty"`
}

// moduleImport is one entry of modules/_imports.cue's `imports:` list.
type moduleImport struct {
	Module  string         `json:"module"`
	Enabled bool           `json:"enabled"`
	Sources []moduleSource `json:"sources"`
}

// moduleImportsCuePath is the top-level CUE field _imports.cue declares its
// list under, mirroring how render.go looks up "output"/"outputs".
const moduleImportsCuePath = "imports"

// parseModuleImports reads addon.ModuleImports (modules/_imports.cue, if the
// addon has one) and returns its enabled imports. An addon with no
// modules/_imports.cue returns a nil slice and a nil error — callers must
// not treat that as an error case.
func parseModuleImports(addon *InstallPackage, args map[string]interface{}) ([]moduleImport, error) {
	if addon.ModuleImports.Data == "" {
		return nil, nil
	}

	var imports []moduleImport
	r := addonCueTemplateRender{addon: addon, inputArgs: args}
	if err := r.toObject(addon.ModuleImports.Data, moduleImportsCuePath, &imports); err != nil {
		return nil, fmt.Errorf("error parsing %s: %w", ModuleImportsFileName, err)
	}

	var enabled []moduleImport
	for _, imp := range imports {
		if !imp.Enabled {
			continue
		}
		enabled = append(enabled, imp)
	}
	return enabled, nil
}
