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
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copyMinimalModule copies the minimal baseline fixture into a fresh temp
// directory and returns its path, so each test case can mutate its own
// private copy without disturbing the checked-in fixture or other cases.
func copyMinimalModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS("testdata/modules/minimal")))
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func TestParseModule_WellFormedModule(t *testing.T) {
	mod, err := ParseModuleDir("testdata/modules/s3")
	require.NoError(t, err)
	require.NotNil(t, mod)

	assert.Equal(t, "s3", mod.Name)
	assert.Equal(t, "1.0.0", mod.Version)

	require.NotEmpty(t, mod.XRD)
	assert.Equal(t, "CompositeResourceDefinition", mod.XRD["kind"])
	xrdMetadata, ok := mod.XRD["metadata"].(map[string]interface{})
	require.True(t, ok, "xrd metadata must be a map")
	assert.Equal(t, "xs3.objectstore.atmos.guidewire.com", xrdMetadata["name"])

	require.Contains(t, mod.Lines, "v1")
	line := mod.Lines["v1"]
	assert.Equal(t, "v1", line.APIVersion)

	require.NotEmpty(t, line.Composition)
	assert.Equal(t, "Composition", line.Composition["kind"])
	compMetadata, ok := line.Composition["metadata"].(map[string]interface{})
	require.True(t, ok, "composition metadata must be a map")
	assert.Equal(t, "s3.objectstore.atmos.guidewire.com", compMetadata["name"])

	require.Len(t, line.Definitions, 1)
	def := line.Definitions[0]
	defMetadata, ok := def["metadata"].(map[string]interface{})
	require.True(t, ok, "definition metadata must be a map")
	assert.Equal(t, "atmos-s3-v1", defMetadata["name"])
}

// TestParseModule_StructuralAndIdentityFailures starts from one minimal
// valid module, copied fresh per case, and breaks exactly one thing per
// case — since ParseModule stops at the first error, each fixture only
// needs to differ from a valid module by the single part under test.
func TestParseModule_StructuralAndIdentityFailures(t *testing.T) {
	cases := []struct {
		name          string
		mutate        func(t *testing.T, dir string)
		wantErrSubstr string
	}{
		{
			name: "missing _module.cue",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.Remove(filepath.Join(dir, "_module.cue")))
			},
			wantErrSubstr: "_module.cue",
		},
		{
			name: "missing module field",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "_module.cue"), `version: "1.0.0"`+"\n")
			},
			wantErrSubstr: "_module.cue",
		},
		{
			name: "missing version field",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "_module.cue"), `module: "minimal"`+"\n")
			},
			wantErrSubstr: "_module.cue",
		},
		{
			name: "non-semver version",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "_module.cue"), "module: \"minimal\"\nversion: \"latest\"\n")
			},
			wantErrSubstr: "_module.cue",
		},
		{
			name: "line missing _version.cue",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.Remove(filepath.Join(dir, "v1", "_version.cue")))
			},
			wantErrSubstr: "_version.cue",
		},
		{
			name: "line with empty definitions/",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.Remove(filepath.Join(dir, "v1", "definitions", "widget.yaml")))
			},
			wantErrSubstr: "definitions",
		},
		{
			name: "module with no API lines",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.RemoveAll(filepath.Join(dir, "v1")))
			},
			wantErrSubstr: "no API lines",
		},
		{
			name: "unsupported definition extension",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.Remove(filepath.Join(dir, "v1", "definitions", "widget.yaml")))
				writeFile(t, filepath.Join(dir, "v1", "definitions", "widget.txt"), "not a definition\n")
			},
			wantErrSubstr: "widget.txt",
		},
		{
			name: "duplicate apiVersion across line directories",
			mutate: func(t *testing.T, dir string) {
				require.NoError(t, os.CopyFS(filepath.Join(dir, "v1x"), os.DirFS(filepath.Join(dir, "v1"))))
			},
			wantErrSubstr: "duplicate line",
		},
		{
			name: "malformed apiVersion",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "v1", "_version.cue"), `apiVersion: "1.0"`+"\n")
			},
			wantErrSubstr: "_version.cue",
		},
		{
			name: "definition with empty metadata.name",
			mutate: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "v1", "definitions", "widget.yaml"), "apiVersion: core.oam.dev/v1beta1\nkind: ComponentDefinition\nmetadata:\n  namespace: vela-system\n")
			},
			wantErrSubstr: "widget.yaml",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := copyMinimalModule(t)
			c.mutate(t, dir)

			mod, err := ParseModuleDir(dir)
			require.Error(t, err)
			require.Nil(t, mod)
			assert.Contains(t, err.Error(), c.wantErrSubstr)
		})
	}
}

// TestParseModule_OptionalAuxiliaryResources covers modules that ship no
// Crossplane infra tier: the module-wide XRD and a line's Composition are
// both optional. A module with neither is still valid as long as its
// identity and definitions are well-formed.
func TestParseModule_OptionalAuxiliaryResources(t *testing.T) {
	t.Run("module without an XRD", func(t *testing.T) {
		dir := copyMinimalModule(t)
		require.NoError(t, os.Remove(filepath.Join(dir, "auxiliary", "xrd.yaml")))

		mod, err := ParseModuleDir(dir)
		require.NoError(t, err)
		require.NotNil(t, mod)

		assert.Nil(t, mod.XRD)
		require.Contains(t, mod.Lines, "v1")
		line := mod.Lines["v1"]
		assert.NotEmpty(t, line.Composition)
		require.Len(t, line.Definitions, 1)
	})

	t.Run("line without a Composition", func(t *testing.T) {
		dir := copyMinimalModule(t)
		require.NoError(t, os.Remove(filepath.Join(dir, "v1", "auxiliary", "composition.yaml")))

		mod, err := ParseModuleDir(dir)
		require.NoError(t, err)
		require.NotNil(t, mod)

		assert.NotEmpty(t, mod.XRD)
		require.Contains(t, mod.Lines, "v1")
		line := mod.Lines["v1"]
		assert.Nil(t, line.Composition)
		require.Len(t, line.Definitions, 1)
	})
}

func TestParseModule_FS(t *testing.T) {
	fsys := fstest.MapFS{
		"_module.cue":                    {Data: []byte(`module: "s3"` + "\n" + `version: "1.0.0"`)},
		"auxiliary/xrd.yaml":             {Data: []byte("apiVersion: apiextensions.crossplane.io/v1\nkind: CompositeResourceDefinition\nmetadata:\n  name: xs3\n")},
		"v1/_version.cue":                {Data: []byte(`apiVersion: "v1"`)},
		"v1/auxiliary/composition.yaml":  {Data: []byte("apiVersion: apiextensions.crossplane.io/v1\nkind: Composition\nmetadata:\n  name: s3\n")},
		"v1/definitions/bucket.yaml":     {Data: []byte("apiVersion: core.oam.dev/v1beta1\nkind: ComponentDefinition\nmetadata:\n  name: atmos-s3-v1\n")},
	}
	mod, err := ParseModule(fsys)
	require.NoError(t, err)
	require.Equal(t, "s3", mod.Name)
	require.Equal(t, "1.0.0", mod.Version)
	require.NotNil(t, mod.XRD)
	require.Contains(t, mod.Lines, "v1")
	require.NotNil(t, mod.Lines["v1"].Composition)
	require.Len(t, mod.Lines["v1"].Definitions, 1)
}
