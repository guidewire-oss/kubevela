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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/chart"
)

// closedPortRegistryURL points listPortableOCICatalog* at a loopback port
// nothing listens on, so listOCITagsWithTransport fails to dial rather than
// answering "repository does not exist" -- exercising the "unavailable" branch
// deterministically without any real network dependency.
const closedPortRegistryURL = "127.0.0.1:1/addon"

// TestListPortableOCICatalogWrappers covers listPortableOCICatalog and
// listPortableOCICatalogWithPlainHTTP, which only select a transport before
// delegating to listPortableOCICatalogWithTransport.
func TestListPortableOCICatalogWrappers(t *testing.T) {
	t.Run("https", func(t *testing.T) {
		_, err := listPortableOCICatalog(context.Background(), closedPortRegistryURL, "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "portable OCI addon catalog is unavailable")
	})

	t.Run("plain http", func(t *testing.T) {
		_, err := listPortableOCICatalogWithPlainHTTP(context.Background(), closedPortRegistryURL, "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "portable OCI addon catalog is unavailable")
	})
}

// TestConfirmPortableCatalogAbsent covers the wrapper that re-probes the
// catalog repository before a rewrite. A dial failure is not a confirmed
// absence, so it must be refused.
func TestConfirmPortableCatalogAbsent(t *testing.T) {
	for _, plainHTTP := range []bool{true, false} {
		err := confirmPortableCatalogAbsent(&OCIAddonSource{URL: closedPortRegistryURL}, plainHTTP)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot confirm whether")
	}
}

// TestUpdateOCIAddonCatalog pins the early error branch: when the existing
// catalog cannot be read (neither the portable catalog nor the registry
// catalog enumeration is reachable), the update must refuse rather than
// silently rebuild the catalog from an empty list. A nil *registry.Client is
// safe here because the function never reaches client.Push on this path.
func TestUpdateOCIAddonCatalog(t *testing.T) {
	addonMeta := &chart.Metadata{Name: "fluxcd", Description: "Flux"}

	for _, plainHTTP := range []bool{true, false} {
		err := updateOCIAddonCatalog(nil, &OCIAddonSource{URL: closedPortRegistryURL}, addonMeta, plainHTTP)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refusing to rewrite the OCI addon catalog: cannot read the existing catalog")
	}
}
