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

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart/loader"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	pkgaddon "github.com/oam-dev/kubevela/pkg/addon"
	"github.com/oam-dev/kubevela/pkg/module"
)

// fakeItem implements pkgaddon.Item.
type fakeItem struct{ path, typ string }

func (f fakeItem) GetType() string { return f.typ }
func (f fakeItem) GetPath() string { return f.path }
func (f fakeItem) GetName() string { return f.path }

// fakeReader implements pkgaddon.AsyncReader over an in-memory file map keyed by
// path relative to the modules root (i.e. "<module>/<rel>"). RelativePath
// returns that same path — the reader-agnostic form readerFS feeds to ReadFile.
type fakeReader struct{ files map[string]string }

func (r fakeReader) ListAddonMeta() (map[string]pkgaddon.SourceMeta, error) {
	byModule := map[string]*pkgaddon.SourceMeta{}
	for p := range r.files {
		mod := p[:indexSlash(p)]
		sm := byModule[mod]
		if sm == nil {
			sm = &pkgaddon.SourceMeta{Name: mod}
			byModule[mod] = sm
		}
		sm.Items = append(sm.Items, fakeItem{path: p, typ: pkgaddon.FileType})
	}
	out := map[string]pkgaddon.SourceMeta{}
	for k, v := range byModule {
		out[k] = *v
	}
	return out, nil
}

func (r fakeReader) ReadFile(p string) (string, error) { return r.files[p], nil }

func (r fakeReader) RelativePath(item pkgaddon.Item) string { return item.GetPath() }

func indexSlash(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return i
		}
	}
	return len(s)
}

// fakeStore is an pkgaddon.RegistryDataStore over an in-memory slice. Unknown names
// return a k8s NotFound (as the real ConfigMap-backed store does), so
// module.ResolveRegistry takes its not-found path.
type fakeStore struct{ regs []pkgaddon.Registry }

func (s fakeStore) GetRegistry(_ context.Context, name string) (pkgaddon.Registry, error) {
	for i := range s.regs {
		if s.regs[i].Name == name {
			return s.regs[i], nil
		}
	}
	return pkgaddon.Registry{}, apierrors.NewNotFound(schema.GroupResource{Resource: "Registry"}, name)
}

func (s fakeStore) ListRegistries(_ context.Context) ([]pkgaddon.Registry, error) { return s.regs, nil }

func (s fakeStore) AddRegistry(context.Context, pkgaddon.Registry) error { return nil }

func (s fakeStore) UpdateRegistry(context.Context, pkgaddon.Registry) error { return nil }

func (s fakeStore) DeleteRegistry(context.Context, string) error { return nil }

func gitRegistry(name string) pkgaddon.Registry {
	return pkgaddon.Registry{Name: name, Git: &pkgaddon.GitAddonSource{URL: "https://example.com/repo", Path: "module"}}
}

func newServiceWithFakes(store pkgaddon.RegistryDataStore, files map[string]string) *Service {
	s := NewService(store)
	s.newReader = func(_ *pkgaddon.Registry) (pkgaddon.AsyncReader, error) { return fakeReader{files: files}, nil }
	return s
}

func TestFetchModule_Git(t *testing.T) {
	files := map[string]string{
		"s3/_module.cue":                "module: \"s3\"\nversion: \"1.0.0\"",
		"s3/v1/_version.cue":            "apiVersion: \"v1\"",
		"s3/v1/definitions/bucket.yaml": "apiVersion: core.oam.dev/v1beta1\nkind: ComponentDefinition\nmetadata:\n  name: atmos-s3-v1\n",
	}
	s := newServiceWithFakes(fakeStore{regs: []pkgaddon.Registry{gitRegistry("catalog")}}, files)

	mod, err := s.FetchModule(context.Background(), "catalog", "s3", "")
	require.NoError(t, err)
	require.Equal(t, "s3", mod.Name)
	require.Equal(t, "1.0.0", mod.Version)
	require.Contains(t, mod.Lines, "v1")
}

// TestFetchModule_Git_IgnoresVersion asserts a git source resolves the same
// module regardless of what version is requested: git has no tag concept, so
// it always pulls from the repository's default branch (the path-based read
// this fake reader already simulates), silently ignoring version.
func TestFetchModule_Git_IgnoresVersion(t *testing.T) {
	files := map[string]string{
		"s3/_module.cue":                "module: \"s3\"\nversion: \"1.0.0\"",
		"s3/v1/_version.cue":            "apiVersion: \"v1\"",
		"s3/v1/definitions/bucket.yaml": "apiVersion: core.oam.dev/v1beta1\nkind: ComponentDefinition\nmetadata:\n  name: atmos-s3-v1\n",
	}
	s := newServiceWithFakes(fakeStore{regs: []pkgaddon.Registry{gitRegistry("catalog")}}, files)

	mod, err := s.FetchModule(context.Background(), "catalog", "s3", "9.9.9-does-not-exist")
	require.NoError(t, err)
	require.Equal(t, "s3", mod.Name)
	require.Equal(t, "1.0.0", mod.Version)
}

func TestFetchModule_EmptyNameResolvesSole(t *testing.T) {
	files := map[string]string{
		"s3/_module.cue":                "module: \"s3\"\nversion: \"1.0.0\"",
		"s3/v1/_version.cue":            "apiVersion: \"v1\"",
		"s3/v1/definitions/bucket.yaml": "apiVersion: core.oam.dev/v1beta1\nkind: ComponentDefinition\nmetadata:\n  name: atmos-s3-v1\n",
	}
	s := newServiceWithFakes(fakeStore{regs: []pkgaddon.Registry{gitRegistry("only")}}, files)

	mod, err := s.FetchModule(context.Background(), "", "s3", "")
	require.NoError(t, err)
	require.Equal(t, "s3", mod.Name)
}

func TestFetchModule_EmptyNameAmbiguous(t *testing.T) {
	s := newServiceWithFakes(fakeStore{regs: []pkgaddon.Registry{gitRegistry("a"), gitRegistry("b")}}, nil)

	_, err := s.FetchModule(context.Background(), "", "s3", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "a")
	require.Contains(t, err.Error(), "b")
}

func TestFetchModule_UnknownRegistry(t *testing.T) {
	s := newServiceWithFakes(fakeStore{regs: []pkgaddon.Registry{gitRegistry("catalog")}}, nil)

	_, err := s.FetchModule(context.Background(), "missing", "s3", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing")
	require.Contains(t, err.Error(), "catalog") // lists configured registries (reused module.NotFoundError)
	require.ErrorIs(t, err, module.ErrRegistryNotFound)
}

func TestFetchModule_RejectsUnsupportedSource(t *testing.T) {
	// A helm entry can live in the shared ConfigMap; module.ResolveRegistry must
	// reject it before any fetch. Verifies fetch honors the git/OCI-only scope.
	helmReg := pkgaddon.Registry{Name: "legacy", Helm: &pkgaddon.HelmSource{URL: "https://charts.example.com"}}
	s := newServiceWithFakes(fakeStore{regs: []pkgaddon.Registry{helmReg}}, nil)

	_, err := s.FetchModule(context.Background(), "legacy", "s3", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "git and OCI")
}

func TestFetchModule_EmptyNameDefaultsToCatalog(t *testing.T) {
	// With several registries and no name, the one named "catalog" wins (the
	// reused module.ResolveRegistry default policy).
	files := map[string]string{
		"s3/_module.cue":                "module: \"s3\"\nversion: \"1.0.0\"",
		"s3/v1/_version.cue":            "apiVersion: \"v1\"",
		"s3/v1/definitions/bucket.yaml": "apiVersion: core.oam.dev/v1beta1\nkind: ComponentDefinition\nmetadata:\n  name: atmos-s3-v1\n",
	}
	s := newServiceWithFakes(fakeStore{regs: []pkgaddon.Registry{gitRegistry("other"), gitRegistry("catalog")}}, files)

	mod, err := s.FetchModule(context.Background(), "", "s3", "")
	require.NoError(t, err)
	require.Equal(t, "s3", mod.Name)
}

func TestFetchModule_ModuleNotFound(t *testing.T) {
	files := map[string]string{
		"other/_module.cue": "module: \"other\"\nversion: \"1.0.0\"",
	}
	s := newServiceWithFakes(fakeStore{regs: []pkgaddon.Registry{gitRegistry("catalog")}}, files)

	_, err := s.FetchModule(context.Background(), "catalog", "s3", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "s3")
	require.Contains(t, err.Error(), "catalog")
	require.ErrorIs(t, err, module.ErrModuleNotFound)
}

func ociRegistry(name string) pkgaddon.Registry {
	return pkgaddon.Registry{Name: name, OCI: &pkgaddon.OCIAddonSource{}}
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

	s := NewService(fakeStore{regs: []pkgaddon.Registry{ociRegistry("oci")}})
	s.pullChart = func(_ context.Context, _ *pkgaddon.Registry, _, _ string) ([]*loader.BufferedFile, error) {
		return bufs, nil
	}

	mod, err := s.FetchModule(context.Background(), "oci", "s3", "")
	require.NoError(t, err)
	require.Equal(t, "s3", mod.Name)
	require.Equal(t, "1.0.0", mod.Version)
	require.Contains(t, mod.Lines, "v1")
	require.Len(t, mod.Lines["v1"].Definitions, 1)
}

// TestFetchModule_OCI_PassesRequestedVersion asserts a non-empty version
// requested by the caller reaches the OCI puller unchanged, so an exact tag
// can be pinned end to end.
func TestFetchModule_OCI_PassesRequestedVersion(t *testing.T) {
	bufs := []*loader.BufferedFile{
		{Name: "s3/_module.cue", Data: []byte("module: \"s3\"\nversion: \"1.2.0\"")},
		{Name: "s3/v1/_version.cue", Data: []byte("apiVersion: \"v1\"")},
		{Name: "s3/v1/definitions/bucket.yaml", Data: []byte("apiVersion: core.oam.dev/v1beta1\nkind: ComponentDefinition\nmetadata:\n  name: atmos-s3-v1\n")},
	}

	var gotVersion string
	s := NewService(fakeStore{regs: []pkgaddon.Registry{ociRegistry("oci")}})
	s.pullChart = func(_ context.Context, _ *pkgaddon.Registry, _, version string) ([]*loader.BufferedFile, error) {
		gotVersion = version
		return bufs, nil
	}

	mod, err := s.FetchModule(context.Background(), "oci", "s3", "1.2.0")
	require.NoError(t, err)
	require.Equal(t, "1.2.0", gotVersion)
	require.Equal(t, "1.2.0", mod.Version)
}

// TestFetchModule_OCI_UnknownVersionFails asserts an OCI puller error (what the
// real registry client returns for a tag that does not exist) surfaces as a
// FetchModule error naming the module, before any parse is attempted.
func TestFetchModule_OCI_UnknownVersionFails(t *testing.T) {
	s := NewService(fakeStore{regs: []pkgaddon.Registry{ociRegistry("oci")}})
	s.pullChart = func(_ context.Context, _ *pkgaddon.Registry, _, version string) ([]*loader.BufferedFile, error) {
		return nil, errors.Errorf("failed to pull addon chart s3:%s: manifest unknown", version)
	}

	mod, err := s.FetchModule(context.Background(), "oci", "s3", "9.9.9-does-not-exist")
	require.Error(t, err)
	require.Nil(t, mod)
	require.Contains(t, err.Error(), "s3")
	require.Contains(t, err.Error(), "9.9.9-does-not-exist")
}
