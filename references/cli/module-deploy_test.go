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

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	velatypes "github.com/oam-dev/kubevela/apis/types"
	pkgaddon "github.com/oam-dev/kubevela/pkg/addon"
	pkgmodule "github.com/oam-dev/kubevela/pkg/module"
	"github.com/oam-dev/kubevela/pkg/utils/common"
	cmdutil "github.com/oam-dev/kubevela/pkg/utils/util"
)

func TestBuildModuleApplication(t *testing.T) {
	app, err := buildModuleApplication("s3", "catalog", "vela-system")
	require.NoError(t, err)

	assert.Equal(t, "module-s3-deploy", app.Name)
	assert.Equal(t, "vela-system", app.Namespace)
	assert.Equal(t, "core.oam.dev/v1beta1", app.APIVersion)
	assert.Equal(t, "Application", app.Kind)

	require.Len(t, app.Spec.Components, 1)
	comp := app.Spec.Components[0]
	assert.Equal(t, "s3", comp.Name)
	assert.Equal(t, "module", comp.Type)

	require.NotNil(t, comp.Properties)
	var props map[string]string
	require.NoError(t, json.Unmarshal(comp.Properties.Raw, &props))
	assert.Equal(t, map[string]string{
		"module":    "s3",
		"registry":  "catalog",
		"namespace": "vela-system",
	}, props)
}

func TestExpectedModuleTiers(t *testing.T) {
	testCases := map[string]struct {
		mod  *pkgmodule.Module
		want []string
	}{
		"xrd, one line with composition and definitions": {
			mod: &pkgmodule.Module{
				Name: "s3",
				XRD:  map[string]interface{}{"kind": "CompositeResourceDefinition"},
				Lines: map[string]pkgmodule.Line{
					"v1": {
						APIVersion:  "v1",
						Enabled:     true,
						Composition: map[string]interface{}{"kind": "Composition"},
						Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}},
					},
				},
			},
			want: []string{"s3-xrd", "s3-v1-comp", "s3-v1-defs"},
		},
		"disabled lines are skipped": {
			mod: &pkgmodule.Module{
				Name: "s3",
				Lines: map[string]pkgmodule.Line{
					"v1": {APIVersion: "v1", Enabled: true, Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}}},
					"v2": {APIVersion: "v2", Enabled: false, Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}}},
				},
			},
			want: []string{"s3-v1-defs"},
		},
		"lines are sorted lexically": {
			mod: &pkgmodule.Module{
				Name: "s3",
				Lines: map[string]pkgmodule.Line{
					"v2":  {APIVersion: "v2", Enabled: true, Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}}},
					"v10": {APIVersion: "v10", Enabled: true, Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}}},
				},
			},
			want: []string{"s3-v10-defs", "s3-v2-defs"},
		},
		"no xrd and no composition": {
			mod: &pkgmodule.Module{
				Name: "kro",
				Lines: map[string]pkgmodule.Line{
					"v1": {APIVersion: "v1", Enabled: true, Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}}},
				},
			},
			want: []string{"kro-v1-defs"},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, expectedModuleTiers(tc.mod))
		})
	}
}

func TestModuleAppNames(t *testing.T) {
	assert.Equal(t, "module-s3-deploy", moduleDeployAppName("s3"))
	assert.Equal(t, "module-s3", ownedModuleAppName("s3"))
	assert.NotEqual(t, moduleDeployAppName("s3"), ownedModuleAppName("s3"))
}

// moduleDeployClient returns a fake client seeded with a module registry
// ConfigMap holding the named registries, so ResolveRegistry can run without a
// cluster.
func moduleDeployClient(t *testing.T, registries ...string) client.Client {
	t.Helper()
	data := "{}"
	if len(registries) > 0 {
		entries := make(map[string]pkgaddon.Registry, len(registries))
		for _, name := range registries {
			entries[name] = pkgaddon.Registry{
				Name: name,
				Git:  &pkgaddon.GitAddonSource{URL: "https://github.com/kubevela/catalog", Path: "module"},
			}
		}
		raw, err := json.Marshal(entries)
		require.NoError(t, err)
		data = string(raw)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pkgmodule.ModuleRegistryConfigMap,
			Namespace: velatypes.DefaultKubeVelaNS,
		},
		Data: map[string]string{"registries": data},
	}
	return fake.NewClientBuilder().WithScheme(common.Scheme).WithObjects(cm).Build()
}

// stubModule is the module the fetch seam returns in tests: an XRD plus one
// enabled line with a Composition and one definition.
func stubModule() *pkgmodule.Module {
	return &pkgmodule.Module{
		Name:    "s3",
		Version: "1.0.0",
		XRD:     map[string]interface{}{"kind": "CompositeResourceDefinition"},
		Lines: map[string]pkgmodule.Line{
			"v1": {
				APIVersion:  "v1",
				Enabled:     true,
				Composition: map[string]interface{}{"kind": "Composition"},
				Definitions: []map[string]interface{}{{"kind": "ComponentDefinition"}},
			},
		},
	}
}

// countApplications returns how many Applications exist on a client.
func countApplications(t *testing.T, cli client.Client) int {
	t.Helper()
	var apps v1beta1.ApplicationList
	require.NoError(t, cli.List(context.Background(), &apps))
	return len(apps.Items)
}

func TestModuleDeployDryRun(t *testing.T) {
	cli := moduleDeployClient(t, "catalog")
	var out bytes.Buffer
	o := &moduleDeployOptions{
		module:    "s3",
		registry:  "catalog",
		namespace: velatypes.DefaultKubeVelaNS,
		dryRun:    true,
		fetch: func(_ context.Context, registry, moduleName string) (*pkgmodule.Module, error) {
			assert.Equal(t, "catalog", registry)
			assert.Equal(t, "s3", moduleName)
			return stubModule(), nil
		},
	}

	require.NoError(t, o.run(context.Background(), cli, &out))

	var printed v1beta1.Application
	require.NoError(t, yaml.Unmarshal(out.Bytes(), &printed))
	assert.Equal(t, "module-s3-deploy", printed.Name)
	require.Len(t, printed.Spec.Components, 1)
	assert.Equal(t, "module", printed.Spec.Components[0].Type)
	assert.Equal(t, 0, countApplications(t, cli))
}

func TestModuleDeployFailsBeforeApply(t *testing.T) {
	testCases := map[string]struct {
		registry    string
		registries  []string
		fetch       func(ctx context.Context, registry, moduleName string) (*pkgmodule.Module, error)
		wantErrPart string
	}{
		"unknown registry": {
			registry:    "missing",
			registries:  []string{"catalog"},
			fetch:       func(_ context.Context, _, _ string) (*pkgmodule.Module, error) { return stubModule(), nil },
			wantErrPart: `module registry "missing" not found`,
		},
		"no registry configured": {
			registry:    "",
			registries:  nil,
			fetch:       func(_ context.Context, _, _ string) (*pkgmodule.Module, error) { return stubModule(), nil },
			wantErrPart: "no module registry is configured",
		},
		"module not in registry": {
			registry:   "catalog",
			registries: []string{"catalog"},
			fetch: func(_ context.Context, _, _ string) (*pkgmodule.Module, error) {
				return nil, fmt.Errorf(`module "s4" not found in registry "catalog"`)
			},
			wantErrPart: `module "s4" not found`,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			cli := moduleDeployClient(t, tc.registries...)
			o := &moduleDeployOptions{
				module:    "s3",
				registry:  tc.registry,
				namespace: velatypes.DefaultKubeVelaNS,
				fetch:     tc.fetch,
			}

			err := o.run(context.Background(), cli, &bytes.Buffer{})

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErrPart)
			assert.Equal(t, 0, countApplications(t, cli), "nothing may be applied when validation fails")
		})
	}
}

func TestModuleDeployResolvesDefaultRegistry(t *testing.T) {
	cli := moduleDeployClient(t, "catalog")
	var out bytes.Buffer
	o := &moduleDeployOptions{
		module:    "s3",
		registry:  "",
		namespace: velatypes.DefaultKubeVelaNS,
		dryRun:    true,
		fetch: func(_ context.Context, registry, _ string) (*pkgmodule.Module, error) {
			assert.Equal(t, "catalog", registry, "the resolved registry is passed to fetch")
			return stubModule(), nil
		},
	}

	require.NoError(t, o.run(context.Background(), cli, &out))

	var printed v1beta1.Application
	require.NoError(t, yaml.Unmarshal(out.Bytes(), &printed))
	var props map[string]string
	require.NoError(t, json.Unmarshal(printed.Spec.Components[0].Properties.Raw, &props))
	assert.Equal(t, "catalog", props["registry"], "the manifest pins the resolved registry")
}

func TestNewModuleDeployCommandFlags(t *testing.T) {
	cmd := NewModuleDeployCommand(common.Args{}, cmdutil.IOStreams{})
	assert.Equal(t, "deploy", strings.Split(cmd.Use, " ")[0])
	for _, flag := range []string{"registry", "dry-run", "timeout"} {
		assert.NotNil(t, cmd.Flags().Lookup(flag), "flag %q must exist", flag)
	}
	assert.Error(t, cmd.Args(cmd, []string{}), "the module name is required")
	assert.NoError(t, cmd.Args(cmd, []string{"s3"}))
}

func TestModuleCommandMountsDeploy(t *testing.T) {
	cmd := NewModuleCommand(common.Args{}, "", cmdutil.IOStreams{})
	names := []string{}
	for _, sub := range cmd.Commands() {
		names = append(names, strings.Split(sub.Use, " ")[0])
	}
	assert.Contains(t, names, "deploy")
}
