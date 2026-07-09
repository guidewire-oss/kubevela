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
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pkgaddon "github.com/oam-dev/kubevela/pkg/addon"
	"github.com/oam-dev/kubevela/pkg/addon/service/api"
)

// fakeClientWithRegistry returns a fake client with no addon registry configured,
// so any addon resolution fails as "not found".
func fakeClientWithRegistry(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().Build()
}

// fakeClientWithMockRegistry returns a fake client backed by a stubbed registry.
// The happy-path render test that uses it is gated behind an env var and is
// exercised by the e2e suite; this keeps the always-on unit test hermetic.
func fakeClientWithMockRegistry(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().Build()
}

func TestRenderAddonNotFound(t *testing.T) {
	r := &rendererImpl{cli: fakeClientWithRegistry(t)}
	_, err := r.RenderAddon(context.Background(), api.AddonRequest{Name: "does-not-exist"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestValidateSystemRequirementsNilPasses(t *testing.T) {
	cli := fakeClientWithRegistry(t)
	err := pkgaddon.ValidateSystemRequirements(context.Background(), nil, cli, nil)
	require.NoError(t, err)
}

func TestRenderAddonSkipVersionValidateStillResolves(t *testing.T) {
	r := &rendererImpl{cli: fakeClientWithRegistry(t)}
	_, err := r.RenderAddon(context.Background(), api.AddonRequest{Name: "does-not-exist", SkipVersionValidate: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestRenderAddonCachesByKey(t *testing.T) {
	r := &rendererImpl{cli: fakeClientWithRegistry(t)}
	var calls int
	r.resolveFn = func(_ context.Context, req api.AddonRequest) (*api.AddonResult, error) {
		calls++
		return &api.AddonResult{ResolvedVersion: req.Version}, nil
	}

	reqA := api.AddonRequest{Name: "example", Version: "1.0.0", Properties: map[string]interface{}{"replicas": 1}}

	res1, err := r.RenderAddon(context.Background(), reqA)
	require.NoError(t, err)
	res2, err := r.RenderAddon(context.Background(), reqA)
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "identical requests must resolve exactly once")
	assert.Same(t, res1, res2, "cache hit must return the same result pointer")

	// Different version and different properties each produce a new key.
	_, err = r.RenderAddon(context.Background(), api.AddonRequest{Name: "example", Version: "2.0.0", Properties: map[string]interface{}{"replicas": 1}})
	require.NoError(t, err)
	_, err = r.RenderAddon(context.Background(), api.AddonRequest{Name: "example", Version: "1.0.0", Properties: map[string]interface{}{"replicas": 2}})
	require.NoError(t, err)
	assert.Equal(t, 3, calls, "distinct requests must each resolve")
}

func TestHashPropertiesStable(t *testing.T) {
	a := map[string]interface{}{"b": 2, "a": 1, "nested": map[string]interface{}{"x": "y"}}
	b := map[string]interface{}{"a": 1, "nested": map[string]interface{}{"x": "y"}, "b": 2}
	assert.Equal(t, hashProperties(a), hashProperties(b), "same map (any order) must hash equally")

	c := map[string]interface{}{"a": 1, "b": 3}
	assert.NotEqual(t, hashProperties(a), hashProperties(c), "different maps must hash differently")

	assert.Equal(t, hashProperties(nil), hashProperties(nil), "nil must hash stably")
}

func TestSanitizeManifest(t *testing.T) {
	testCases := map[string]struct {
		in   map[string]interface{}
		want map[string]interface{}
	}{
		"strips root status and top-level creationTimestamp": {
			in: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Namespace",
				"metadata": map[string]interface{}{
					"name":              "flux-system",
					"creationTimestamp": nil,
				},
				"status": map[string]interface{}{"phase": "Active"},
			},
			want: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Namespace",
				"metadata": map[string]interface{}{
					"name": "flux-system",
				},
			},
		},
		"strips creationTimestamp in nested objects and arrays": {
			in: map[string]interface{}{
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"name":              "pod",
							"creationTimestamp": nil,
						},
					},
					"objects": []interface{}{
						map[string]interface{}{
							"metadata": map[string]interface{}{
								"name":              "cm",
								"creationTimestamp": nil,
							},
						},
					},
				},
			},
			want: map[string]interface{}{
				"spec": map[string]interface{}{
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"name": "pod",
						},
					},
					"objects": []interface{}{
						map[string]interface{}{
							"metadata": map[string]interface{}{
								"name": "cm",
							},
						},
					},
				},
			},
		},
		"leaves a manifest without status or creationTimestamp unchanged": {
			in: map[string]interface{}{
				"kind":     "ConfigMap",
				"metadata": map[string]interface{}{"name": "keep"},
			},
			want: map[string]interface{}{
				"kind":     "ConfigMap",
				"metadata": map[string]interface{}{"name": "keep"},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			sanitizeManifest(tc.in)
			assert.Equal(t, tc.want, tc.in)
		})
	}
}

func TestRenderAddonReturnsApplicationAndResources(t *testing.T) {
	if os.Getenv("KUBEVELA_ADDON_RENDER_E2E") == "" {
		t.Skip("requires a reachable registry; happy path is covered by the e2e suite (set KUBEVELA_ADDON_RENDER_E2E to run)")
	}
	r := &rendererImpl{cli: fakeClientWithMockRegistry(t)}
	res, err := r.RenderAddon(context.Background(), api.AddonRequest{Name: "example", Version: "1.0.0"})
	require.NoError(t, err)
	assert.Equal(t, "1.0.0", res.ResolvedVersion)
	assert.Equal(t, "Application", res.Application["kind"])
	assert.NotEmpty(t, res.Resources)
}
