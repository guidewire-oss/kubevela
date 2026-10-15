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

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/module"
)

// placed returns an auxiliary object carrying the placement annotation.
func placed(kind, name, placement string) map[string]interface{} {
	o := auxObject(kind, name)
	o["metadata"].(map[string]interface{})["annotations"] = map[string]interface{}{
		types.AnnoModulePlacement: placement,
	}
	return o
}

// placementModule has a hub XRD at module level and, in line v1, one hub and
// one runtime auxiliary object plus a definition.
func placementModule() *module.Module {
	return &module.Module{
		Name:      "s3",
		Version:   "1.0.0",
		Auxiliary: []map[string]interface{}{placed("CompositeResourceDefinition", "xbuckets", types.ModulePlacementHub)},
		Lines: map[string]module.Line{"v1": {
			APIVersion: "v1",
			Enabled:    true,
			Auxiliary: []map[string]interface{}{
				placed("Composition", "xbuckets-v1", types.ModulePlacementHub),
				placed("Deployment", "agent", types.ModulePlacementRuntime),
			},
			Definitions: []map[string]interface{}{definition("bucket")},
		}},
	}
}

func tierNames(comps []map[string]interface{}) []string {
	out := make([]string, 0, len(comps))
	for _, c := range comps {
		out = append(out, c["name"].(string))
	}
	return out
}

func TestRenderApplication_RuntimeObjectsGetTheirOwnTier(t *testing.T) {
	app, err := RenderApplication(placementModule(), "", WithRuntimeClusters("eu-1", "us-1"))
	require.NoError(t, err)

	comps := components(t, app)
	require.Equal(t, []string{"s3-aux", "s3-v1-aux", "s3-v1-aux-runtime", "s3-v1-defs"}, tierNames(comps))

	// Hub and runtime siblings share a dependency; the next tier waits on both.
	require.Equal(t, []interface{}{"s3-aux"}, comps[1]["dependsOn"])
	require.Equal(t, []interface{}{"s3-aux"}, comps[2]["dependsOn"])
	require.Equal(t, []interface{}{"s3-v1-aux", "s3-v1-aux-runtime"}, comps[3]["dependsOn"])

	objs := comps[2]["properties"].(map[string]interface{})["objects"].([]interface{})
	require.Len(t, objs, 1)
	require.Equal(t, "agent", objs[0].(map[string]interface{})["metadata"].(map[string]interface{})["name"])
}

func TestRenderApplication_ModuleLevelRuntimeTierGatesEveryLine(t *testing.T) {
	mod := placementModule()
	mod.Auxiliary = append(mod.Auxiliary, placed("DaemonSet", "node-agent", types.ModulePlacementRuntime))
	app, err := RenderApplication(mod, "", WithRuntimeClusters("eu-1"))
	require.NoError(t, err)

	comps := components(t, app)
	require.Equal(t, "s3-aux-runtime", comps[1]["name"])
	// The line tiers wait on both module-level tiers.
	require.Equal(t, []interface{}{"s3-aux", "s3-aux-runtime"}, comps[2]["dependsOn"])
}

func TestRenderApplication_RuntimeObjectsAddADeployModuleStep(t *testing.T) {
	app, err := RenderApplication(placementModule(), "", WithRuntimeClusters("eu-1", "us-1"))
	require.NoError(t, err)

	spec := app["spec"].(map[string]interface{})
	wf, ok := spec["workflow"].(map[string]interface{})
	require.True(t, ok, "a module with runtime objects needs a workflow")
	steps := wf["steps"].([]interface{})
	require.Len(t, steps, 1)
	step := steps[0].(map[string]interface{})
	require.Equal(t, "deploy-module", step["type"])
	require.Equal(t, []interface{}{"eu-1", "us-1"}, step["properties"].(map[string]interface{})["clusters"])
}

func TestRenderApplication_NoRuntimeObjectsLeavesTheOutputAlone(t *testing.T) {
	// Even with clusters supplied, a hub-only module keeps today's shape.
	app, err := RenderApplication(fixtureModule(), "", WithRuntimeClusters("eu-1"))
	require.NoError(t, err)

	require.Equal(t, []string{"s3-aux", "s3-v1-aux", "s3-v1-defs"}, tierNames(components(t, app)))
	_, hasWorkflow := app["spec"].(map[string]interface{})["workflow"]
	require.False(t, hasWorkflow)
}

func TestRenderApplication_RuntimeObjectsNeedClusters(t *testing.T) {
	_, err := RenderApplication(placementModule(), "")
	require.ErrorContains(t, err, "runtime")
	require.ErrorContains(t, err, "clusters")
}

func TestRenderApplication_UnknownPlacementIsRejected(t *testing.T) {
	mod := placementModule()
	mod.Lines["v1"].Auxiliary[0]["metadata"].(map[string]interface{})["annotations"] = map[string]interface{}{
		types.AnnoModulePlacement: "edge",
	}
	_, err := RenderApplication(mod, "", WithRuntimeClusters("eu-1"))
	require.ErrorContains(t, err, "edge")
}

func TestRenderApplication_DefinitionsStayOnTheHubWhateverTheyAreAnnotated(t *testing.T) {
	mod := placementModule()
	line := mod.Lines["v1"]
	line.Definitions = []map[string]interface{}{placed("ComponentDefinition", "bucket", types.ModulePlacementRuntime)}
	mod.Lines["v1"] = line

	app, err := RenderApplication(mod, "", WithRuntimeClusters("eu-1"))
	require.NoError(t, err)

	comps := components(t, app)
	defs := comps[len(comps)-1]
	require.Equal(t, "s3-v1-defs", defs["name"])
	obj := defs["properties"].(map[string]interface{})["objects"].([]interface{})[0].(map[string]interface{})
	annos := obj["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
	require.NotContains(t, annos, types.AnnoModulePlacement)
}

func TestRenderApplication_PlacementDoesNotMutateTheFetchedModule(t *testing.T) {
	mod := placementModule()
	_, err := RenderApplication(mod, "", WithRuntimeClusters("eu-1"))
	require.NoError(t, err)
	require.Len(t, mod.Lines["v1"].Auxiliary, 2)
	require.Len(t, mod.Auxiliary, 1)
}
