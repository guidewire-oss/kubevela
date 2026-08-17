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

package cli

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pkgmodule "github.com/oam-dev/kubevela/pkg/module"
)

func TestBuildModuleApplication(t *testing.T) {
	app, err := buildModuleApplication("s3", "catalog", "vela-system")
	require.NoError(t, err)

	assert.Equal(t, "module-s3-deploy", app.Name)
	assert.Equal(t, "vela-system", app.Namespace)
	assert.Equal(t, "core.oam.dev/v1beta1", app.APIVersion)
	assert.Equal(t, "Application", app.Kind)

	require.Len(t, app.Spec.Components, 1)
	comp := app.Spec.Components[0]
	assert.Equal(t, "s3", comp.Name)
	assert.Equal(t, "module", comp.Type)

	require.NotNil(t, comp.Properties)
	var props map[string]string
	require.NoError(t, json.Unmarshal(comp.Properties.Raw, &props))
	assert.Equal(t, map[string]string{
		"module":    "s3",
		"registry":  "catalog",
		"namespace": "vela-system",
	}, props)
}

func TestExpectedModuleTiers(t *testing.T) {
	testCases := map[string]struct {
		mod  *pkgmodule.Module
		want []string
	}{
		"xrd, one line with composition and definitions": {
			mod: &pkgmodule.Module{
				Name: "s3",
				XRD:  map[string]interface{}{"kind": "CompositeResourceDefinition"},
				Lines: map[string]pkgmodule.Line{
					"v1": {
						APIVersion:  "v1",
						Enabled:     true,
						Composition: map[string]interface{}{"kind": "Composition"},
						Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}},
					},
				},
			},
			want: []string{"s3-xrd", "s3-v1-comp", "s3-v1-defs"},
		},
		"disabled lines are skipped": {
			mod: &pkgmodule.Module{
				Name: "s3",
				Lines: map[string]pkgmodule.Line{
					"v1": {APIVersion: "v1", Enabled: true, Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}}},
					"v2": {APIVersion: "v2", Enabled: false, Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}}},
				},
			},
			want: []string{"s3-v1-defs"},
		},
		"lines are sorted lexically": {
			mod: &pkgmodule.Module{
				Name: "s3",
				Lines: map[string]pkgmodule.Line{
					"v2":  {APIVersion: "v2", Enabled: true, Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}}},
					"v10": {APIVersion: "v10", Enabled: true, Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}}},
				},
			},
			want: []string{"s3-v10-defs", "s3-v2-defs"},
		},
		"no xrd and no composition": {
			mod: &pkgmodule.Module{
				Name: "kro",
				Lines: map[string]pkgmodule.Line{
					"v1": {APIVersion: "v1", Enabled: true, Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}}},
				},
			},
			want: []string{"kro-v1-defs"},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, expectedModuleTiers(tc.mod))
		})
	}
}

func TestModuleAppNames(t *testing.T) {
	assert.Equal(t, "module-s3-deploy", moduleDeployAppName("s3"))
	assert.Equal(t, "module-s3", ownedModuleAppName("s3"))
	assert.NotEqual(t, moduleDeployAppName("s3"), ownedModuleAppName("s3"))
}
