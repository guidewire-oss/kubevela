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

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart/loader"

	"github.com/oam-dev/kubevela/pkg/addon"
)

// fakeItem implements addon.Item.
type fakeItem struct{ path, typ string }

func (f fakeItem) GetType() string { return f.typ }
func (f fakeItem) GetPath() string { return f.path }
func (f fakeItem) GetName() string { return f.path }

// fakeReader implements addon.AsyncReader over an in-memory file map keyed by
// path relative to the modules root (i.e. "<module>/<rel>"). RelativePath
// returns that same path — the reader-agnostic form readerFS feeds to ReadFile.
type fakeReader struct{ files map[string]string }

func (r fakeReader) ListAddonMeta() (map[string]addon.SourceMeta, error) {
	byModule := map[string]*addon.SourceMeta{}
	for p := range r.files {
		mod := p[:indexSlash(p)]
		sm := byModule[mod]
		if sm == nil {
			sm = &addon.SourceMeta{Name: mod}
			byModule[mod] = sm
		}
		sm.Items = append(sm.Items, fakeItem{path: p, typ: addon.FileType})
	}
	out := map[string]addon.SourceMeta{}
	for k, v := range byModule {
		out[k] = *v
	}
	return out, nil
}

func (r fakeReader) ReadFile(p string) (string, error) { return r.files[p], nil }

func (r fakeReader) RelativePath(item addon.Item) string { return item.GetPath() }

func indexSlash(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return i
		}
	}
	return len(s)
}

type fakeStore struct{ regs []addon.Registry }

func (s fakeStore) GetRegistry(_ context.Context, name string) (addon.Registry, error) {
	for i := range s.regs {
		if s.regs[i].Name == name {
			return s.regs[i], nil
		}
	}
	return addon.Registry{}, addon.ErrRegistryNotExist
}

func (s fakeStore) ListRegistries(_ context.Context) ([]addon.Registry, error) { return s.regs, nil }

func gitRegistry(name string) addon.Registry {
	return addon.Registry{Name: name, Git: &addon.GitAddonSource{URL: "https://example.com/repo", Path: "module"}}
}

func newServiceWithFakes(store ModuleRegistryStore, files map[string]string) *Service {
	s := NewService(store)
	s.newReader = func(_ *addon.Registry) (addon.AsyncReader, error) { return fakeReader{files: files}, nil }
	return s
}

func TestFetchModule_Git(t *testing.T) {
	files := map[string]string{
		"s3/_module.cue":                "module: \"s3\"\nversion: \"1.0.0\"",
		"s3/v1/_version.cue":            "apiVersion: \"v1\"",
		"s3/v1/definitions/bucket.yaml": "apiVersion: core.oam.dev/v1beta1\nkind: ComponentDefinition\nmetadata:\n  name: atmos-s3-v1\n",
	}
	s := newServiceWithFakes(fakeStore{regs: []addon.Registry{gitRegistry("catalog")}}, files)

	mod, err := s.FetchModule(context.Background(), "catalog", "s3")
	require.NoError(t, err)
	require.Equal(t, "s3", mod.Name)
	require.Equal(t, "1.0.0", mod.Version)
	require.Contains(t, mod.Lines, "v1")
}

func TestFetchModule_EmptyNameResolvesSole(t *testing.T) {
	files := map[string]string{
		"s3/_module.cue":                "module: \"s3\"\nversion: \"1.0.0\"",
		"s3/v1/_version.cue":            "apiVersion: \"v1\"",
		"s3/v1/definitions/bucket.yaml": "apiVersion: core.oam.dev/v1beta1\nkind: ComponentDefinition\nmetadata:\n  name: atmos-s3-v1\n",
	}
	s := newServiceWithFakes(fakeStore{regs: []addon.Registry{gitRegistry("only")}}, files)

	mod, err := s.FetchModule(context.Background(), "", "s3")
	require.NoError(t, err)
	require.Equal(t, "s3", mod.Name)
}

func TestFetchModule_EmptyNameAmbiguous(t *testing.T) {
	s := newServiceWithFakes(fakeStore{regs: []addon.Registry{gitRegistry("a"), gitRegistry("b")}}, nil)

	_, err := s.FetchModule(context.Background(), "", "s3")
	require.Error(t, err)
	require.Contains(t, err.Error(), "a")
	require.Contains(t, err.Error(), "b")
}

func TestFetchModule_UnknownRegistry(t *testing.T) {
	s := newServiceWithFakes(fakeStore{regs: []addon.Registry{gitRegistry("catalog")}}, nil)

	_, err := s.FetchModule(context.Background(), "missing", "s3")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing")
}

func TestFetchModule_ModuleNotFound(t *testing.T) {
	files := map[string]string{
		"other/_module.cue": "module: \"other\"\nversion: \"1.0.0\"",
	}
	s := newServiceWithFakes(fakeStore{regs: []addon.Registry{gitRegistry("catalog")}}, files)

	_, err := s.FetchModule(context.Background(), "catalog", "s3")
	require.Error(t, err)
	require.Contains(t, err.Error(), "s3")
	require.Contains(t, err.Error(), "catalog")
}

func ociRegistry(name string) addon.Registry {
	return addon.Registry{Name: name, OCI: &addon.OCIAddonSource{}}
}

// TestFetchModule_OCI_EqualsGit drives the OCI branch (pull -> MemoryReader ->
// readerFS) through an injected puller and asserts it yields the same Module the
// git path does. The real pull is exercised live in the round-trip test.
func TestFetchModule_OCI_EqualsGit(t *testing.T) {
	// A Helm chart carries files prefixed by the chart (module) name — exactly
	// what pullModuleChart returns and what MemoryReader expects.
	bufs := []*loader.BufferedFile{
		{Name: "s3/_module.cue", Data: []byte("module: \"s3\"\nversion: \"1.0.0\"")},
		{Name: "s3/v1/_version.cue", Data: []byte("apiVersion: \"v1\"")},
		{Name: "s3/v1/definitions/bucket.yaml", Data: []byte("apiVersion: core.oam.dev/v1beta1\nkind: ComponentDefinition\nmetadata:\n  name: atmos-s3-v1\n")},
		{Name: "s3/Chart.yaml", Data: []byte("name: s3\nversion: 1.0.0\n")}, // chart wrapper; parser ignores it
	}

	s := NewService(fakeStore{regs: []addon.Registry{ociRegistry("oci")}})
	s.pullChart = func(_ context.Context, _ *addon.Registry, _ string) ([]*loader.BufferedFile, error) {
		return bufs, nil
	}

	mod, err := s.FetchModule(context.Background(), "oci", "s3")
	require.NoError(t, err)
	require.Equal(t, "s3", mod.Name)
	require.Equal(t, "1.0.0", mod.Version)
	require.Contains(t, mod.Lines, "v1")
	require.Len(t, mod.Lines["v1"].Definitions, 1)
}
