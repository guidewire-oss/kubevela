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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/module"
	"github.com/oam-dev/kubevela/pkg/oam"
)

// definition builds a minimal ComponentDefinition map as the parser produces it.
func definition(name string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "core.oam.dev/v1beta1",
		"kind":       "ComponentDefinition",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{},
	}
}

// fixtureModule is an s3 module with a module-wide XRD and one enabled line
// carrying a Composition and one definition.
func fixtureModule() *module.Module {
	return &module.Module{
		Name:    "s3",
		Version: "1.0.0",
		XRD: map[string]interface{}{
			"apiVersion": "apiextensions.crossplane.io/v1",
			"kind":       "CompositeResourceDefinition",
			"metadata":   map[string]interface{}{"name": "xbuckets.aws.platform.io"},
		},
		Lines: map[string]module.Line{
			"v1": {
				APIVersion: "v1",
				Enabled:    true,
				Composition: map[string]interface{}{
					"apiVersion": "apiextensions.crossplane.io/v1",
					"kind":       "Composition",
					"metadata":   map[string]interface{}{"name": "xbuckets-v1"},
				},
				Definitions: []map[string]interface{}{definition("bucket")},
			},
		},
	}
}

// components returns the owned Application's spec.components as typed maps.
func components(t *testing.T, app map[string]interface{}) []map[string]interface{} {
	t.Helper()
	spec, ok := app["spec"].(map[string]interface{})
	require.True(t, ok, "app has no spec")
	raw, ok := spec["components"].([]interface{})
	require.True(t, ok, "app has no spec.components")
	out := make([]map[string]interface{}, 0, len(raw))
	for _, c := range raw {
		m, ok := c.(map[string]interface{})
		require.True(t, ok)
		out = append(out, m)
	}
	return out
}

func TestRenderApplication_OwnedApplicationShape(t *testing.T) {
	app, err := RenderApplication(fixtureModule())
	require.NoError(t, err)

	require.Equal(t, "core.oam.dev/v1beta1", app["apiVersion"])
	require.Equal(t, "Application", app["kind"])
	meta, ok := app["metadata"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, "module-s3", meta["name"])

	comps := components(t, app)
	require.Len(t, comps, 3)
	require.Equal(t, "s3-xrd", comps[0]["name"])
	require.Equal(t, "s3-v1-comp", comps[1]["name"])
	require.Equal(t, "s3-v1-defs", comps[2]["name"])
}

// twoLineModule ships v1 and v2, both with a Composition and a definition.
func twoLineModule(v1Enabled, v2Enabled bool) *module.Module {
	line := func(v string, enabled bool) module.Line {
		return module.Line{
			APIVersion: v,
			Enabled:    enabled,
			Composition: map[string]interface{}{
				"apiVersion": "apiextensions.crossplane.io/v1",
				"kind":       "Composition",
				"metadata":   map[string]interface{}{"name": "xbuckets-" + v},
			},
			Definitions: []map[string]interface{}{definition("bucket")},
		}
	}
	return &module.Module{
		Name:    "s3",
		Version: "1.0.0",
		XRD: map[string]interface{}{
			"apiVersion": "apiextensions.crossplane.io/v1",
			"kind":       "CompositeResourceDefinition",
			"metadata":   map[string]interface{}{"name": "xbuckets.aws.platform.io"},
		},
		Lines: map[string]module.Line{
			"v1": line("v1", v1Enabled),
			"v2": line("v2", v2Enabled),
		},
	}
}

func names(comps []map[string]interface{}) []string {
	out := make([]string, 0, len(comps))
	for _, c := range comps {
		out = append(out, c["name"].(string))
	}
	return out
}

func TestRenderApplication_BothEnabledLinesInstall(t *testing.T) {
	app, err := RenderApplication(twoLineModule(true, true))
	require.NoError(t, err)

	require.Equal(t, []string{
		"s3-xrd",
		"s3-v1-comp", "s3-v1-defs",
		"s3-v2-comp", "s3-v2-defs",
	}, names(components(t, app)))
}

func TestRenderApplication_DisabledLineIsSkipped(t *testing.T) {
	app, err := RenderApplication(twoLineModule(false, true))
	require.NoError(t, err)

	got := names(components(t, app))
	require.Equal(t, []string{"s3-xrd", "s3-v2-comp", "s3-v2-defs"}, got)
	require.NotContains(t, got, "s3-v1-defs")
}

func TestRenderApplication_LinesAreSiblingsNotAChain(t *testing.T) {
	app, err := RenderApplication(twoLineModule(true, true))
	require.NoError(t, err)

	byName := map[string]map[string]interface{}{}
	for _, c := range components(t, app) {
		byName[c["name"].(string)] = c
	}

	// Both lines hang off the XRD; v2 must not wait on v1.
	require.Equal(t, []interface{}{"s3-xrd"}, byName["s3-v1-comp"]["dependsOn"])
	require.Equal(t, []interface{}{"s3-xrd"}, byName["s3-v2-comp"]["dependsOn"])
	require.Equal(t, []interface{}{"s3-v1-comp"}, byName["s3-v1-defs"]["dependsOn"])
	require.Equal(t, []interface{}{"s3-v2-comp"}, byName["s3-v2-defs"]["dependsOn"])
	// The first tier gates on nothing.
	require.NotContains(t, byName["s3-xrd"], "dependsOn")
}

func TestRenderApplication_NoXRDOmitsTheXRDTier(t *testing.T) {
	mod := fixtureModule()
	mod.XRD = nil

	app, err := RenderApplication(mod)
	require.NoError(t, err)

	comps := components(t, app)
	require.Equal(t, []string{"s3-v1-comp", "s3-v1-defs"}, names(comps))
	// With no XRD above it, the Composition tier gates on nothing.
	require.NotContains(t, comps[0], "dependsOn")
}

func TestRenderApplication_NoCompositionOmitsTheCompositionTier(t *testing.T) {
	mod := fixtureModule()
	line := mod.Lines["v1"]
	line.Composition = nil
	mod.Lines["v1"] = line

	app, err := RenderApplication(mod)
	require.NoError(t, err)

	comps := components(t, app)
	require.Equal(t, []string{"s3-xrd", "s3-v1-defs"}, names(comps))
	// The definitions tier falls back to gating on the XRD.
	require.Equal(t, []interface{}{"s3-xrd"}, comps[1]["dependsOn"])
}

func TestRenderApplication_KROStyleModuleHasNoGates(t *testing.T) {
	mod := fixtureModule()
	mod.XRD = nil
	line := mod.Lines["v1"]
	line.Composition = nil
	mod.Lines["v1"] = line

	app, err := RenderApplication(mod)
	require.NoError(t, err)

	comps := components(t, app)
	require.Equal(t, []string{"s3-v1-defs"}, names(comps))
	require.NotContains(t, comps[0], "dependsOn")
}

func TestRenderApplication_AllLinesDisabledYieldsOnlyTheXRDTier(t *testing.T) {
	app, err := RenderApplication(twoLineModule(false, false))
	require.NoError(t, err)

	require.Equal(t, []string{"s3-xrd"}, names(components(t, app)))
}

// TestRenderApplication_TierNamesComeFromTheMapKey documents current behaviour,
// not desired behaviour. The parser keys Lines by the apiVersion declared inside
// v<N>/_version.cue and never checks it against the directory name, so a v1/
// directory whose _version.cue says "v9beta1" lands under that key. The render
// iterates the map, so the mismatch surfaces as tiers named for the declared
// version rather than the directory. Recorded so the behaviour is visible if
// someone hits it; fixing the parser is tracked separately.
func TestRenderApplication_TierNamesComeFromTheMapKey(t *testing.T) {
	mod := fixtureModule()
	line := mod.Lines["v1"]
	delete(mod.Lines, "v1")
	mod.Lines["v9beta1"] = line // declared apiVersion disagreed with the directory

	app, err := RenderApplication(mod)
	require.NoError(t, err)

	require.Equal(t, []string{"s3-xrd", "s3-v9beta1-comp", "s3-v9beta1-defs"}, names(components(t, app)))
}

func TestRenderApplication_TierCarriesReadyCondition(t *testing.T) {
	app, err := RenderApplication(fixtureModule())
	require.NoError(t, err)

	byName := map[string]map[string]interface{}{}
	for _, c := range components(t, app) {
		byName[c["name"].(string)] = c
	}

	xrd := byName["s3-xrd"]
	require.Equal(t, "k8s-objects-ready", xrd["type"])
	xrdProps := xrd["properties"].(map[string]interface{})
	require.Equal(t, "Established", xrdProps["readyConditionType"])
	require.Len(t, xrdProps["objects"], 1)

	// Crossplane's Composition has no status.conditions, so this hop cannot
	// gate on a real condition; empty means healthy once applied.
	comp := byName["s3-v1-comp"]
	require.Equal(t, "k8s-objects-ready", comp["type"])
	require.Equal(t, "", comp["properties"].(map[string]interface{})["readyConditionType"])

	// The definitions tier needs no health policy: nothing gates on it.
	require.Equal(t, "k8s-objects", byName["s3-v1-defs"]["type"])
}

func TestRenderApplication_StampsDefinitionIdentity(t *testing.T) {
	app, err := RenderApplication(fixtureModule())
	require.NoError(t, err)

	var defs []interface{}
	for _, c := range components(t, app) {
		if c["name"] == "s3-v1-defs" {
			defs = c["properties"].(map[string]interface{})["objects"].([]interface{})
		}
	}
	require.Len(t, defs, 1)
	def := defs[0].(map[string]interface{})

	meta := def["metadata"].(map[string]interface{})
	require.Equal(t, "s3-v1-bucket", meta["name"])

	labels := meta["labels"].(map[string]interface{})
	require.Equal(t, "s3", labels[types.LabelDefinitionModule])
	require.Equal(t, "v1", labels[types.LabelDefinitionAPIVersion])
	require.Equal(t, "bucket", labels[types.LabelDefinitionName])
	require.Equal(t, "s3", labels[oam.LabelAddonName])

	annos := meta["annotations"].(map[string]interface{})
	require.Equal(t, "s3-v1-bucket", annos[types.AnnoDefinitionFullName])

	spec := def["spec"].(map[string]interface{})
	require.Equal(t, "s3", spec["module"])
	require.Equal(t, "v1", spec["apiVersion"])
}

func TestRenderApplication_DoesNotMutateTheFetchedModule(t *testing.T) {
	mod := fixtureModule()
	_, err := RenderApplication(mod)
	require.NoError(t, err)

	// The parsed Module is shared and may be cached; the render must copy.
	original := mod.Lines["v1"].Definitions[0]["metadata"].(map[string]interface{})
	require.Equal(t, "bucket", original["name"])
	require.NotContains(t, original, "labels")
}

func TestRenderApplication_TruncatesLongNamesWithAStableHash(t *testing.T) {
	long := strings.Repeat("a", 300)
	mod := fixtureModule()
	line := mod.Lines["v1"]
	line.Definitions = []map[string]interface{}{definition(long)}
	mod.Lines["v1"] = line

	app, err := RenderApplication(mod)
	require.NoError(t, err)
	app2, err := RenderApplication(mod)
	require.NoError(t, err)

	nameOf := func(a map[string]interface{}) string {
		for _, c := range components(t, a) {
			if c["name"] == "s3-v1-defs" {
				objs := c["properties"].(map[string]interface{})["objects"].([]interface{})
				return objs[0].(map[string]interface{})["metadata"].(map[string]interface{})["name"].(string)
			}
		}
		return ""
	}

	got := nameOf(app)
	require.Len(t, got, 253, "an over-long name is truncated to the Kubernetes limit")
	require.Equal(t, got, nameOf(app2), "the hash suffix must be stable across renders")

	// The full name survives on the annotation even when the object name cannot hold it.
	for _, c := range components(t, app) {
		if c["name"] == "s3-v1-defs" {
			objs := c["properties"].(map[string]interface{})["objects"].([]interface{})
			annos := objs[0].(map[string]interface{})["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
			require.Equal(t, "s3-v1-"+long, annos[types.AnnoDefinitionFullName])
		}
	}
}
