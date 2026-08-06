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
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/oam-dev/kubevela/pkg/addon"
	"github.com/oam-dev/kubevela/pkg/module"
)

// TestFetchModule_RoundTrip publishes the s3 testdata module to a real git and a
// real OCI registry, then fetches each back and asserts an equal Module. It skips
// cleanly when the registries or the publish command are unavailable, so the
// default `go test` run (no integration tag) never touches it.
func TestFetchModule_RoundTrip(t *testing.T) {
	gitURL := os.Getenv("MODULE_GIT_REGISTRY")
	ociURL := os.Getenv("MODULE_OCI_REGISTRY")
	if gitURL == "" || ociURL == "" {
		t.Skip("set MODULE_GIT_REGISTRY and MODULE_OCI_REGISTRY to run the round-trip")
	}

	gitMod := publishAndFetch(t, gitURL, addon.Registry{
		Name: "git-live", Git: &addon.GitAddonSource{URL: gitURL, Path: "module"},
	})
	ociMod := publishAndFetch(t, ociURL, addon.Registry{
		Name: "oci-live", OCI: &addon.OCIAddonSource{URL: ociURL},
	})

	require.Equal(t, gitMod.Name, ociMod.Name)
	require.Equal(t, gitMod.Version, ociMod.Version)
	require.Equal(t, len(gitMod.Lines), len(ociMod.Lines))
	require.Contains(t, gitMod.Lines, "v1")
	require.Contains(t, ociMod.Lines, "v1")
}

// publishAndFetch publishes the s3 testdata module to target via
// `vela module publish` (GWCP-106685), then fetches it back through a Service
// pointed at reg and returns the parsed Module. It is a t.Skip until publish has
// landed; the file compiles under `-tags integration` and the default suite
// ignores it entirely.
func publishAndFetch(t *testing.T, target string, reg addon.Registry) *module.Module {
	t.Helper()
	t.Skip("implement once vela module publish (GWCP-106685) has landed")
	// 1. run `vela module publish pkg/module/testdata/modules/s3 --registry <target>`
	//    (skip if the binary/command is unavailable)
	// 2. store := <a ModuleRegistryStore returning reg>; s := NewService(store)
	// 3. mod, err := s.FetchModule(context.Background(), reg.Name, "s3"); require.NoError
	// 4. return mod
	return nil
}
