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

package appfile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/features"
)

func definitionIn(namespace, name, extends, template string) *v1beta1.ComponentDefinition {
	cd := compDef(name, extends, template)
	cd.Namespace = namespace
	return cd
}

func fakeClientWith(objs ...*v1beta1.ComponentDefinition) *fake.ClientBuilder {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b
}

// A parent in the same namespace resolves.
func TestParentInTheSameNamespaceResolves(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.EnableDefinitionInheritance, true)

	cli := fakeClientWith(
		definitionIn("team-a", "base", "", "output: base: true"),
	).Build()

	child := definitionIn("team-a", "child", "base", "super: properties: {}")
	got, err := clusterComponentFetcher(cli, "team-a", nil)(context.Background(), "base")
	require.NoError(t, err)
	require.Equal(t, "team-a", got.Namespace)

	tmpl := &Template{}
	require.NoError(t, resolveComponentChain(context.Background(), tmpl, child,
		clusterComponentFetcher(cli, "team-a", nil)))
	require.Len(t, tmpl.Ancestors, 1)
}

// The escalation this closes: a definition in a user's namespace reaching a
// privileged one in vela-system. The lookup finds it, because capability
// resolution falls back to the system namespace, and it is refused by name.
func TestParentInAnotherNamespaceIsRefused(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.EnableDefinitionInheritance, true)

	cli := fakeClientWith(
		definitionIn("vela-system", "webservice", "", "output: privileged: true"),
	).Build()

	child := definitionIn("team-a", "sneaky", "webservice", "super: properties: {}")
	err := resolveComponentChain(context.Background(), &Template{}, child,
		clusterComponentFetcher(cli, "team-a", nil))

	require.ErrorContains(t, err, "vela-system")
	require.ErrorContains(t, err, "its own namespace")
}

// A definition in vela-system extending another in vela-system is the supported
// way to build on a builtin, and is unaffected.
func TestSystemNamespaceChainResolves(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.EnableDefinitionInheritance, true)

	cli := fakeClientWith(
		definitionIn("vela-system", "webservice", "", "output: base: true"),
	).Build()

	child := definitionIn("vela-system", "tenant-webservice", "webservice", "super: properties: {}")
	tmpl := &Template{}
	require.NoError(t, resolveComponentChain(context.Background(), tmpl, child,
		clusterComponentFetcher(cli, "vela-system", nil)))
	require.Len(t, tmpl.Ancestors, 1)
	require.Equal(t, "webservice", tmpl.Ancestors[0].Name)
}
