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
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsOCIRegistry(t *testing.T) {
	assert.True(t, IsOCIRegistry(Registry{OCI: &OCIAddonSource{URL: "oci://x/y"}}))
	assert.False(t, IsOCIRegistry(Registry{Helm: &HelmSource{URL: "http://x"}}))
	assert.False(t, IsOCIRegistry(Registry{Git: &GitAddonSource{URL: "http://x"}}))
	assert.False(t, IsOCIRegistry(Registry{}))
}

func TestOCIAddonSourceTokenSource(t *testing.T) {
	s := &OCIAddonSource{URL: "oci://reg/addon", Username: "AWS", Token: "secret"}
	// GetTokenSource should return the OCI source
	r := Registry{Name: "ecr", OCI: s}
	require.NotNil(t, r.GetTokenSource())
	assert.Equal(t, "secret", r.GetTokenSource().GetToken())

	// SetTokenSecretRef clears the inline token and records the ref
	s.SetTokenSecretRef("addon-registry-ecr")
	assert.Equal(t, "", s.GetToken())
	assert.Equal(t, "addon-registry-ecr", s.GetTokenSecretRef())

	// SetToken clears the ref
	s.SetToken("t2")
	assert.Equal(t, "t2", s.GetToken())
	assert.Equal(t, "", s.GetTokenSecretRef())

	// SafeCopy hides the token but keeps the ref
	s.SetTokenSecretRef("addon-registry-ecr")
	safe := s.SafeCopy()
	assert.Equal(t, "", safe.Token)
	assert.Equal(t, "addon-registry-ecr", safe.TokenSecretRef)
	assert.Equal(t, "oci://reg/addon", safe.URL)
}

func TestOCIRepoRef(t *testing.T) {
	cases := map[string]struct {
		url, addon         string
		wantRepo, wantHost string
	}{
		"with scheme": {
			url: "oci://reg.example.com/addon", addon: "fluxcd",
			wantRepo: "reg.example.com/addon/fluxcd", wantHost: "reg.example.com",
		},
		"no scheme": {
			url: "reg.example.com/addon", addon: "fluxcd",
			wantRepo: "reg.example.com/addon/fluxcd", wantHost: "reg.example.com",
		},
		"trailing slash": {
			url: "oci://reg.example.com/addon/", addon: "velaux",
			wantRepo: "reg.example.com/addon/velaux", wantHost: "reg.example.com",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			repo, host := ociRepoRef(tc.url, tc.addon)
			assert.Equal(t, tc.wantRepo, repo)
			assert.Equal(t, tc.wantHost, host)
		})
	}
}

// TestOCIRegistryLoadAddon injects a fake puller returning a real addon chart
// archive (a prebuilt fixture), so the OCI load path is exercised without any
// network and without writing artifacts to disk.
func TestOCIRegistryLoadAddon(t *testing.T) {
	data, err := os.ReadFile("./testdata/helm-repo/fluxcd-1.0.0.tgz")
	require.NoError(t, err)

	// Empty version must resolve to the highest semver tag via the tag lister,
	// then pull that exact tag.
	reg := &ociRegistry{
		name:     "ecr",
		url:      "oci://reg.example.com/addon",
		username: "AWS",
		token:    "secret",
		tagsFn: func(_ context.Context, repoRef, host, user, pass string) ([]string, error) {
			assert.Equal(t, "reg.example.com/addon/fluxcd", repoRef)
			assert.Equal(t, "reg.example.com", host)
			// helm's Tags returns semver-sorted, highest first.
			return []string{"3.0.1", "2.0.0", "1.0.0"}, nil
		},
		pullFn: func(_ context.Context, ref, host, user, pass string) ([]byte, error) {
			assert.Equal(t, "reg.example.com/addon/fluxcd:3.0.1", ref)
			assert.Equal(t, "reg.example.com", host)
			assert.Equal(t, "AWS", user)
			assert.Equal(t, "secret", pass)
			return data, nil
		},
	}

	pkg, err := reg.GetAddonInstallPackage(context.Background(), "fluxcd", "")
	require.NoError(t, err)
	require.NotNil(t, pkg)
	assert.Equal(t, "fluxcd", pkg.Name)

	// GetDetailedAddon should stamp the registry name.
	whole, err := reg.GetDetailedAddon(context.Background(), "fluxcd", "")
	require.NoError(t, err)
	assert.Equal(t, "ecr", whole.RegistryName)

	// ListAddon is intentionally unsupported for OCI registries.
	_, err = reg.ListAddon()
	assert.Error(t, err)
}

// TestOCIRegistryExplicitVersion pins a version: no tag listing should happen,
// the exact tag is pulled.
func TestOCIRegistryExplicitVersion(t *testing.T) {
	data, err := os.ReadFile("./testdata/helm-repo/fluxcd-1.0.0.tgz")
	require.NoError(t, err)

	reg := &ociRegistry{
		name: "ecr", url: "oci://reg.example.com/addon", username: "AWS", token: "secret",
		tagsFn: func(_ context.Context, _, _, _, _ string) ([]string, error) {
			t.Fatalf("tag listing must not be called when a version is pinned")
			return nil, nil
		},
		pullFn: func(_ context.Context, ref, _, _, _ string) ([]byte, error) {
			assert.Equal(t, "reg.example.com/addon/fluxcd:3.0.1", ref)
			return data, nil
		},
	}
	pkg, err := reg.GetAddonInstallPackage(context.Background(), "fluxcd", "3.0.1")
	require.NoError(t, err)
	assert.Equal(t, "fluxcd", pkg.Name)
}

// TestOCIRegistryNoTags errors clearly when no semver tags exist.
func TestOCIRegistryNoTags(t *testing.T) {
	reg := &ociRegistry{
		name: "ecr", url: "oci://reg.example.com/addon",
		tagsFn: func(_ context.Context, _, _, _, _ string) ([]string, error) {
			return []string{}, nil
		},
	}
	_, err := reg.GetAddonInstallPackage(context.Background(), "fluxcd", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no")
}
