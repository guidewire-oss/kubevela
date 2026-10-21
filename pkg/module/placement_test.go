/*
Copyright 2021 The KubeVela Authors.

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

package module

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/oam-dev/kubevela/pkg/oam"
)

const placementWidgetDefinition = `apiVersion: core.oam.dev/v1beta1
kind: ComponentDefinition
metadata:
  name: widget
  namespace: vela-system
spec:
  workload:
    definition:
      apiVersion: v1
      kind: ConfigMap
`

// placementTree returns a minimal module tree with v1 and v2 lines, plus the
// extra files given. Both lines carry a valid definition.
func placementTree(t *testing.T, extra map[string]string) string {
	t.Helper()
	files := map[string]string{
		"_module.cue":                "module: \"minimal\"\nversion: \"1.0.0\"\n",
		"v1/_version.cue":            "apiVersion: \"v1\"\n",
		"v1/definitions/widget.yaml": placementWidgetDefinition,
		"v2/_version.cue":            "apiVersion: \"v2\"\n",
		"v2/definitions/widget.yaml": placementWidgetDefinition,
	}
	for k, v := range extra {
		files[k] = v
	}
	return writeModuleTree(t, files)
}

func configMapYAML(name, annotations string) string {
	out := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n"
	if annotations != "" {
		out += "  annotations:\n" + annotations
	}
	return out
}

func annotationsOf(obj map[string]interface{}) map[string]interface{} {
	md, _ := obj["metadata"].(map[string]interface{})
	ann, _ := md["annotations"].(map[string]interface{})
	return ann
}

func findByName(objs []map[string]interface{}, name string) map[string]interface{} {
	for _, obj := range objs {
		md, _ := obj["metadata"].(map[string]interface{})
		if n, _ := md["name"].(string); n == name {
			return obj
		}
	}
	return nil
}

func TestParseModule_PlacementAnnotationKeptOnYAMLAndCUE(t *testing.T) {
	dir := placementTree(t, map[string]string{
		"auxiliary/a.yaml": configMapYAML("a", "    resource.oam.dev/placement: hub\n"),
		"auxiliary/b.cue": `apiVersion: "v1"
kind:       "ConfigMap"
metadata: {
	name: "b"
	annotations: "resource.oam.dev/placement": "hub"
}
`,
	})
	mod, err := ParseModuleDir(dir)
	require.NoError(t, err)

	a := findByName(mod.Auxiliary, "a")
	require.NotNil(t, a)
	assert.Equal(t, oam.PlacementHub, annotationsOf(a)[oam.AnnotationPlacement])
	b := findByName(mod.Auxiliary, "b")
	require.NotNil(t, b)
	assert.Equal(t, oam.PlacementHub, annotationsOf(b)[oam.AnnotationPlacement])
}

func TestParseModule_NoMarkersLeavesEverythingUnset(t *testing.T) {
	mod, err := ParseModuleDir(minimalModuleDir(t))
	require.NoError(t, err)

	assert.Nil(t, mod.Placement)
	assert.Empty(t, mod.Warnings)
	assert.Nil(t, mod.Lines["v1"].Placement)
	assert.Nil(t, annotationsOf(findByKind(mod.Auxiliary, "CompositeResourceDefinition")))
}

func TestParseModule_PlacementDefaults(t *testing.T) {
	dir := placementTree(t, map[string]string{
		"_module.cue":     "module: \"minimal\"\nversion: \"1.0.0\"\nplacement: \"hub\"\n",
		"v2/_version.cue": "apiVersion: \"v2\"\nplacement: \"runtime\"\n",
	})
	mod, err := ParseModuleDir(dir)
	require.NoError(t, err)

	require.NotNil(t, mod.Placement)
	assert.Equal(t, oam.PlacementHub, *mod.Placement)
	require.NotNil(t, mod.Lines["v2"].Placement)
	assert.Equal(t, oam.PlacementRuntime, *mod.Lines["v2"].Placement)
	assert.Nil(t, mod.Lines["v1"].Placement, "a line default must not leak into another line")
}

func TestParseModule_HubFolderStampsAnnotation(t *testing.T) {
	dir := placementTree(t, map[string]string{
		"auxiliary/plain.yaml":               configMapYAML("plain", ""),
		"auxiliary/hub/stamped.yaml":         configMapYAML("stamped", ""),
		"auxiliary/hub/keeps.yaml":           configMapYAML("keeps", "    team: platform\n"),
		"auxiliary/hub/multi.yaml":           configMapYAML("multi-1", "") + "---\n" + configMapYAML("multi-2", ""),
		"auxiliary/hub/from-cue.cue":         "apiVersion: \"v1\"\nkind: \"ConfigMap\"\nmetadata: name: \"from-cue\"\n",
		"v1/auxiliary/hub/line-stamped.yaml": configMapYAML("line-stamped", ""),
		"v1/auxiliary/line-plain.yaml":       configMapYAML("line-plain", ""),
	})
	mod, err := ParseModuleDir(dir)
	require.NoError(t, err)

	for _, name := range []string{"stamped", "keeps", "multi-1", "multi-2", "from-cue"} {
		obj := findByName(mod.Auxiliary, name)
		require.NotNil(t, obj, name)
		assert.Equal(t, oam.PlacementHub, annotationsOf(obj)[oam.AnnotationPlacement], name)
	}
	assert.Equal(t, "platform", annotationsOf(findByName(mod.Auxiliary, "keeps"))["team"],
		"stamping must keep annotations the author already wrote")
	assert.Nil(t, annotationsOf(findByName(mod.Auxiliary, "plain")), "objects outside hub/ are not stamped")

	line := mod.Lines["v1"]
	assert.Equal(t, oam.PlacementHub, annotationsOf(findByName(line.Auxiliary, "line-stamped"))[oam.AnnotationPlacement])
	assert.Nil(t, annotationsOf(findByName(line.Auxiliary, "line-plain")))
	assert.Empty(t, mod.Warnings)
}

// An object's own annotation outranks the hub/ folder it sits in, so an explicit
// runtime marker is kept and the author is told about the conflict.
func TestParseModule_ExplicitRuntimeAnnotationBeatsHubFolder(t *testing.T) {
	dir := placementTree(t, map[string]string{
		"auxiliary/hub/x.yaml":           configMapYAML("x", "    resource.oam.dev/placement: runtime\n"),
		"v1/auxiliary/hub/line-obj.yaml": configMapYAML("line-obj", "    resource.oam.dev/placement: runtime\n"),
		"auxiliary/hub/same.yaml":        configMapYAML("same", "    resource.oam.dev/placement: hub\n"),
		"auxiliary/hub/unmarked.yaml":    configMapYAML("unmarked", ""),
	})
	mod, err := ParseModuleDir(dir)
	require.NoError(t, err)

	assert.Equal(t, oam.PlacementRuntime, annotationsOf(findByName(mod.Auxiliary, "x"))[oam.AnnotationPlacement])
	assert.Equal(t, oam.PlacementRuntime, annotationsOf(findByName(mod.Lines["v1"].Auxiliary, "line-obj"))[oam.AnnotationPlacement])
	assert.Equal(t, oam.PlacementHub, annotationsOf(findByName(mod.Auxiliary, "same"))[oam.AnnotationPlacement])
	assert.Equal(t, oam.PlacementHub, annotationsOf(findByName(mod.Auxiliary, "unmarked"))[oam.AnnotationPlacement])

	require.Len(t, mod.Warnings, 2, "only the two runtime-in-hub conflicts warn")
	assert.Contains(t, mod.Warnings[0], "auxiliary/hub/x.yaml")
	assert.Contains(t, mod.Warnings[1], "v1/auxiliary/hub/line-obj.yaml")
}

func TestParseModule_HubFolderRejectsInvalidAnnotation(t *testing.T) {
	dir := placementTree(t, map[string]string{
		"auxiliary/hub/x.yaml": configMapYAML("x", "    resource.oam.dev/placement: edge\n"),
	})
	_, err := ParseModuleDir(dir)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "auxiliary/hub/x.yaml")
	assert.Contains(t, err.Error(), "edge")
}

func TestParseModule_NullCUEFileInHubIsAnError(t *testing.T) {
	dir := placementTree(t, map[string]string{
		"auxiliary/hub/null.cue": "null\n",
	})

	var err error
	require.NotPanics(t, func() {
		_, err = ParseModuleDir(dir)
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "auxiliary/hub/null.cue")
}

func TestParseModule_NestedFolderInHubWarns(t *testing.T) {
	dir := placementTree(t, map[string]string{
		"auxiliary/hub/crds/foo.yaml":  configMapYAML("foo", ""),
		"v1/auxiliary/hub/top.yaml":    configMapYAML("top", ""),
		"v1/auxiliary/hub/deep/b.yaml": configMapYAML("b", ""),
	})
	mod, err := ParseModuleDir(dir)
	require.NoError(t, err)

	require.Len(t, mod.Warnings, 2)
	assert.Contains(t, mod.Warnings[0], "auxiliary/hub/crds")
	assert.Contains(t, mod.Warnings[1], "v1/auxiliary/hub/deep")
	assert.Nil(t, findByName(mod.Auxiliary, "foo"))
}

func TestParseModule_HubIsNotRecursed(t *testing.T) {
	dir := placementTree(t, map[string]string{
		"auxiliary/hub/nested/deep.yaml": configMapYAML("deep", ""),
		"auxiliary/hub/top.yaml":         configMapYAML("top", ""),
	})
	mod, err := ParseModuleDir(dir)
	require.NoError(t, err)

	assert.NotNil(t, findByName(mod.Auxiliary, "top"))
	assert.Nil(t, findByName(mod.Auxiliary, "deep"), "hub/ must not recurse into nested folders")
}

func TestParseModule_UnrecognizedAuxiliarySubfolderWarns(t *testing.T) {
	dir := placementTree(t, map[string]string{
		"auxiliary/spoke/a.yaml":      configMapYAML("a", ""),
		"v1/auxiliary/runtime/b.yaml": configMapYAML("b", ""),
	})
	mod, err := ParseModuleDir(dir)
	require.NoError(t, err)

	require.Len(t, mod.Warnings, 2)
	assert.Contains(t, mod.Warnings[0], "auxiliary/spoke")
	assert.Contains(t, mod.Warnings[1], "v1/auxiliary/runtime")
	assert.Nil(t, findByName(mod.Auxiliary, "a"), "an unrecognized folder is still not read")
}

func TestParseModule_HubFolderUnderDefinitionsIsNoOp(t *testing.T) {
	dir := placementTree(t, map[string]string{
		"v1/definitions/hub/extra.yaml": placementWidgetDefinition,
	})
	mod, err := ParseModuleDir(dir)
	require.NoError(t, err)

	assert.Empty(t, mod.Warnings)
	assert.Len(t, mod.Lines["v1"].Definitions, 1)
}

func TestParseModule_PlacementMarkerOnDefinitionAccepted(t *testing.T) {
	for _, value := range []string{oam.PlacementHub, oam.PlacementRuntime} {
		def := strings.Replace(placementWidgetDefinition, "  namespace: vela-system\n",
			"  namespace: vela-system\n  annotations:\n    resource.oam.dev/placement: "+value+"\n", 1)
		dir := placementTree(t, map[string]string{"v1/definitions/widget.yaml": def})
		mod, err := ParseModuleDir(dir)
		require.NoError(t, err, value)
		assert.Len(t, mod.Lines["v1"].Definitions, 1, value)
		assert.Empty(t, mod.Warnings, value)
	}
}

func TestParseModule_InvalidPlacementValueRejected(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		wantInErr []string
	}{
		{
			name:      "module default",
			files:     map[string]string{"_module.cue": "module: \"minimal\"\nversion: \"1.0.0\"\nplacement: \"spoke\"\n"},
			wantInErr: []string{"_module.cue", "spoke"},
		},
		{
			name:      "line default",
			files:     map[string]string{"v2/_version.cue": "apiVersion: \"v2\"\nplacement: \"cloud\"\n"},
			wantInErr: []string{"v2/_version.cue", "cloud"},
		},
		{
			name:      "module auxiliary annotation",
			files:     map[string]string{"auxiliary/a.yaml": configMapYAML("a", "    resource.oam.dev/placement: edge\n")},
			wantInErr: []string{"auxiliary/a.yaml", "edge"},
		},
		{
			name:      "line auxiliary annotation",
			files:     map[string]string{"v1/auxiliary/a.yaml": configMapYAML("a", "    resource.oam.dev/placement: edge\n")},
			wantInErr: []string{"v1/auxiliary/a.yaml", "edge"},
		},
		{
			name:      "annotation inside a multi-document file",
			files:     map[string]string{"auxiliary/a.yaml": configMapYAML("ok", "") + "---\n" + configMapYAML("bad", "    resource.oam.dev/placement: edge\n")},
			wantInErr: []string{"auxiliary/a.yaml", "edge"},
		},
		{
			name:      "non-string annotation value",
			files:     map[string]string{"auxiliary/a.yaml": configMapYAML("a", "    resource.oam.dev/placement: 7\n")},
			wantInErr: []string{"auxiliary/a.yaml"},
		},
		{
			name:      "non-string default",
			files:     map[string]string{"_module.cue": "module: \"minimal\"\nversion: \"1.0.0\"\nplacement: 3\n"},
			wantInErr: []string{"_module.cue"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseModuleDir(placementTree(t, c.files))
			require.Error(t, err)
			for _, want := range c.wantInErr {
				assert.Contains(t, filepath.ToSlash(err.Error()), want)
			}
		})
	}
}

func TestValidatePlacement(t *testing.T) {
	assert.NoError(t, validatePlacement(oam.PlacementHub, "x.yaml"))
	assert.NoError(t, validatePlacement(oam.PlacementRuntime, "x.yaml"))

	for _, bad := range []string{"", "Hub", "spoke", " hub"} {
		err := validatePlacement(bad, "modules/x/_module.cue")
		require.Error(t, err, "%q", bad)
		assert.Contains(t, err.Error(), "_module.cue")
	}
}
