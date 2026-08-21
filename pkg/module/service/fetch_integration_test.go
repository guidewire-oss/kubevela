//go:build integration
// +build integration

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
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	pkgaddon "github.com/oam-dev/kubevela/pkg/addon"
	"github.com/oam-dev/kubevela/pkg/module"
)

// TestFetchModule_RoundTrip publishes the s3 testdata module to a real git and a
// real OCI registry, then fetches each back. Each leg runs in its own t.Run so a
// skip in one (t.Skip calls runtime.Goexit, which would otherwise abort the whole
// test function on its single goroutine) cannot prevent the other from running.
// It skips cleanly when a registry is unavailable, so the default `go test` run
// (no integration tag) never touches it.
//
// There is no cross-registry assertion comparing the git and OCI results: git
// catalog publish is out of scope for GWCP-106685, so nothing here can publish to
// a git registry yet. Restore that comparison once a story adds it.
func TestFetchModule_RoundTrip(t *testing.T) {
	t.Run("git", func(t *testing.T) {
		gitURL := os.Getenv("MODULE_GIT_REGISTRY")
		if gitURL == "" {
			t.Skip("set MODULE_GIT_REGISTRY to run the round-trip")
		}
		publishAndFetch(t, gitURL, pkgaddon.Registry{
			Name: "git-live", Git: &pkgaddon.GitAddonSource{URL: gitURL, Path: "module"},
		})
	})

	t.Run("oci", func(t *testing.T) {
		ociURL := os.Getenv("MODULE_OCI_REGISTRY")
		if ociURL == "" {
			t.Skip("set MODULE_OCI_REGISTRY to run the round-trip")
		}
		source, err := module.ParseModuleDir("../testdata/modules/s3")
		require.NoError(t, err)

		mod := publishAndFetch(t, ociURL, pkgaddon.Registry{
			Name: "oci-live", OCI: &pkgaddon.OCIAddonSource{URL: ociURL},
		})
		require.Equal(t, source.Name, mod.Name)
		require.Equal(t, source.Version, mod.Version)
		require.Contains(t, mod.Lines, "v1")
	})
}

// publishAndFetch packages and publishes the s3 testdata module through the
// pkg/module packages (the same code `vela module publish` (GWCP-106685)
// drives), then fetches it back through a Service pointed at reg and returns
// the parsed Module. Git-registry targets are out of scope for this story and
// skip cleanly; only the OCI path publishes and fetches for real.
func publishAndFetch(t *testing.T, _ string, reg pkgaddon.Registry) *module.Module {
	t.Helper()
	ctx := context.Background()

	art, err := module.PackageModule("../testdata/modules/s3", "")
	require.NoError(t, err)
	if reg.OCI == nil {
		t.Skip("publish supports OCI/ECR only; git catalog publish is out of scope (GWCP-106685)")
	}
	require.NoError(t, pkgaddon.PushOCIChart(ctx, reg, art.Module.Name, art.Tag, art.Archive))

	mod, err := NewService(fakeStore{regs: []pkgaddon.Registry{reg}}).FetchModule(ctx, reg.Name, art.Module.Name, "")
	require.NoError(t, err)
	return mod
}
