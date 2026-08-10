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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/cue/process"
	"github.com/oam-dev/kubevela/pkg/module/service/api"
)

// moduleTemplateDefPath is the module ComponentDefinition under test.
const moduleTemplateDefPath = "../../../vela-templates/definitions/internal/component/module.cue"

type fakeModuleRenderer struct {
	req api.ModuleRequest
	res *api.ModuleResult
	err error
}

func (f *fakeModuleRenderer) RenderModule(_ context.Context, req api.ModuleRequest) (*api.ModuleResult, error) {
	f.req = req
	return f.res, f.err
}

// TestModuleComponentDefinitionRenders compiles the real module
// ComponentDefinition through the WorkloadCompiler with a fake renderer
// installed, and asserts the component's output is the owned Application.
func TestModuleComponentDefinitionRenders(t *testing.T) {
	prev := api.DefaultRenderer()
	t.Cleanup(func() { api.SetDefaultRenderer(prev) })

	fake := &fakeModuleRenderer{
		res: &api.ModuleResult{
			Application: map[string]interface{}{
				"apiVersion": "core.oam.dev/v1beta1",
				"kind":       "Application",
				"metadata":   map[string]interface{}{"name": "module-s3", "namespace": "vela-system"},
			},
		},
	}
	api.SetDefaultRenderer(fake)

	abstractTemplate := extractAbstractTemplate(t, moduleTemplateDefPath)

	ctx := process.NewContext(process.ContextData{
		AppName:         "myapp",
		CompName:        "s3",
		Namespace:       "default",
		AppRevisionName: "myapp-v1",
		ClusterVersion:  types.ClusterVersion{Minor: "19+"},
	})

	wt := NewWorkloadAbstractEngine("module")
	require.NoError(t, wt.Complete(ctx, abstractTemplate, map[string]interface{}{
		"module":   "s3",
		"registry": "catalog",
	}))

	// The render service received the component parameters.
	assert.Equal(t, "s3", fake.req.Module)
	assert.Equal(t, "catalog", fake.req.Registry)

	base, assists := ctx.Output()
	require.NotNil(t, base)
	baseObj, err := base.Unstructured()
	require.NoError(t, err)
	assert.Equal(t, "Application", baseObj.GetKind())
	assert.Equal(t, "core.oam.dev/v1beta1", baseObj.GetAPIVersion())
	assert.Equal(t, "module-s3", baseObj.GetName())
	assert.Empty(t, assists, "module component must emit no outputs")
}

// TestModuleComponentDefinitionDefaults verifies module falls back to the
// component name and registry defaults to empty (the configured default).
func TestModuleComponentDefinitionDefaults(t *testing.T) {
	prev := api.DefaultRenderer()
	t.Cleanup(func() { api.SetDefaultRenderer(prev) })

	fake := &fakeModuleRenderer{
		res: &api.ModuleResult{
			Application: map[string]interface{}{
				"apiVersion": "core.oam.dev/v1beta1",
				"kind":       "Application",
				"metadata":   map[string]interface{}{"name": "module-postgres"},
			},
		},
	}
	api.SetDefaultRenderer(fake)

	abstractTemplate := extractAbstractTemplate(t, moduleTemplateDefPath)

	ctx := process.NewContext(process.ContextData{
		AppName:         "myapp",
		CompName:        "postgres",
		Namespace:       "default",
		AppRevisionName: "myapp-v1",
		ClusterVersion:  types.ClusterVersion{Minor: "19+"},
	})

	wt := NewWorkloadAbstractEngine("module")
	require.NoError(t, wt.Complete(ctx, abstractTemplate, map[string]interface{}{}))

	assert.Equal(t, "postgres", fake.req.Module)
	assert.Equal(t, "", fake.req.Registry)
}
