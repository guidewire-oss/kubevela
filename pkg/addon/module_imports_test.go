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

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseModuleImportsNoFile(t *testing.T) {
	imports, err := parseModuleImports(&InstallPackage{}, nil)
	assert.NoError(t, err)
	assert.Nil(t, imports)
}

func TestParseModuleImportsFiltersDisabled(t *testing.T) {
	cueContent := `imports: [
	{module: "aws-s3", enabled: true, sources: [{registry: "oam-modules"}]},
	{module: "aws-rds", enabled: false, sources: [{registry: "oam-modules"}]},
]`
	addon := &InstallPackage{ModuleImports: ElementFile{Data: cueContent, Name: ModuleImportsFileName}}

	imports, err := parseModuleImports(addon, nil)
	assert.NoError(t, err)
	assert.Len(t, imports, 1)
	assert.Equal(t, "aws-s3", imports[0].Module)
	assert.Equal(t, "oam-modules", imports[0].Sources[0].Registry)
}

func TestParseModuleImportsInvalidCue(t *testing.T) {
	addon := &InstallPackage{ModuleImports: ElementFile{Data: `not: valid: :: cue`, Name: ModuleImportsFileName}}
	_, err := parseModuleImports(addon, nil)
	assert.Error(t, err)
}
