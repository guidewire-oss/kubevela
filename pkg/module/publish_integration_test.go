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

package module

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/require"

	"github.com/oam-dev/kubevela/pkg/addon"
)

// TestPublishRoundTripInProcessRegistry publishes the s3 fixture to an
// in-process OCI registry, reads the manifest back to check the tag and the
// annotations, then pulls the artifact through the same code the module fetch
// uses and asserts an equal Module. It proves the round trip end to end
// without any external dependency, so it always runs under -tags integration.
func TestPublishRoundTripInProcessRegistry(t *testing.T) {
	server := httptest.NewServer(ggcrregistry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")

	reg := addon.Registry{Name: "local", OCI: &addon.OCIAddonSource{URL: "http://" + host + "/modules"}}
	publishAndAssert(t, reg, host+"/modules", name.Insecure)
}

// TestPublishRoundTripECR runs the same assertions against a real ECR
// repository prefix when MODULE_ECR_REGISTRY is set, for example
// 123456789012.dkr.ecr.us-west-2.amazonaws.com/modules, or against any
// plain-HTTP registry via an "http://" prefix (e.g. a port-forwarded
// registry:2). Credentials come from the docker credential chain, so
// `aws ecr get-login-password | docker login` or docker-credential-ecr-login
// must be in place for ECR. The repository <prefix>/s3 must already exist.
// It skips cleanly when the environment variable is unset, so the default
// (non-integration) suite and CI runs without ECR access never touch a real
// registry.
func TestPublishRoundTripECR(t *testing.T) {
	target := os.Getenv("MODULE_ECR_REGISTRY")
	if target == "" {
		t.Skip("set MODULE_ECR_REGISTRY to publish against a real ECR registry")
	}
	reg := addon.Registry{Name: "ecr", OCI: &addon.OCIAddonSource{URL: target}}

	// Strip whichever scheme the target carries (oci://, https://, http://,
	// or none) to get the bare repoPrefix name.NewTag expects, and pass
	// name.Insecure only for an explicit http:// target so the manifest read
	// matches the scheme the push itself used; a real ECR target keeps
	// verifying HTTPS.
	repoPrefix := target
	var refOpts []name.Option
	switch {
	case strings.HasPrefix(target, "http://"):
		repoPrefix = strings.TrimPrefix(target, "http://")
		refOpts = append(refOpts, name.Insecure)
	case strings.HasPrefix(target, "https://"):
		repoPrefix = strings.TrimPrefix(target, "https://")
	case strings.HasPrefix(target, "oci://"):
		repoPrefix = strings.TrimPrefix(target, "oci://")
	}
	publishAndAssert(t, reg, repoPrefix, refOpts...)
}

// publishAndAssert packages the s3 testdata module, pushes it to reg, then
// verifies the round trip from both directions: the raw OCI manifest (tag and
// annotations) via go-containerregistry, and the parsed Module via the same
// PullOCIChartFiles path the module fetch service uses.
func publishAndAssert(t *testing.T, reg addon.Registry, repoPrefix string, refOpts ...name.Option) {
	t.Helper()
	ctx := context.Background()

	source, err := ParseModuleDir("testdata/modules/s3")
	require.NoError(t, err)

	art, err := PackageModule("testdata/modules/s3", "")
	require.NoError(t, err)
	require.NoError(t, addon.PushOCIChart(ctx, reg, source.Name, art.Tag, art.Archive))

	ref, err := name.NewTag(repoPrefix+"/"+source.Name+":"+art.Tag, refOpts...)
	require.NoError(t, err)
	desc, err := remote.Get(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithContext(ctx))
	require.NoError(t, err)
	manifest, err := desc.Image()
	require.NoError(t, err)
	mf, err := manifest.Manifest()
	require.NoError(t, err)
	require.Equal(t, source.Name, mf.Annotations[AnnotationModule])
	require.Equal(t, "v1", mf.Annotations[AnnotationLines])
	require.Equal(t, "v1", mf.Annotations[AnnotationEnabledLines])

	files, err := addon.PullOCIChartFiles(ctx, reg, source.Name, art.Tag)
	require.NoError(t, err)
	pulled := map[string][]byte{}
	for _, f := range files {
		pulled[f.Name] = f.Data
	}
	fetched, err := ParseModule(fstestMapFS(pulled))
	require.NoError(t, err)
	require.Equal(t, source, fetched)
}
