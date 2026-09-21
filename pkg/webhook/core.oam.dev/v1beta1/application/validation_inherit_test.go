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

package application

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

func componentDef(namespace, name, extends string) *v1beta1.ComponentDefinition {
	return &v1beta1.ComponentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1beta1.ComponentDefinitionSpec{
			Extends:   extends,
			Schematic: &common.Schematic{CUE: &common.CUE{Template: "output: {}"}},
		},
	}
}

func handlerWith(defs ...*v1beta1.ComponentDefinition) *ValidatingHandler {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, d := range defs {
		b = b.WithObjects(d)
	}
	return &ValidatingHandler{Client: b.Build()}
}

// The escalation: a user barred from `webservice` defines their own type that
// extends it and names that instead. The permission check must see webservice.
func TestPermissionCheckSeesWhatATypeExtends(t *testing.T) {
	h := handlerWith(
		componentDef("team-a", "webservice", ""),
		componentDef("team-a", "sneaky", "webservice"),
	)
	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
		Spec: v1beta1.ApplicationSpec{
			Components: []common.ApplicationComponent{{Name: "c", Type: "sneaky"}},
		},
	}

	usage := collectDefinitionUsage(app)
	require.NotContains(t, usage.componentTypes, "webservice", "before: only the named type")

	h.addInheritedTypes(context.Background(), app, usage)
	require.Contains(t, usage.componentTypes, "sneaky")
	require.Contains(t, usage.componentTypes, "webservice",
		"the parent is checked too, or extending launders the permission")
}

// A whole chain is covered, not just the immediate parent.
func TestPermissionCheckWalksTheWholeChain(t *testing.T) {
	h := handlerWith(
		componentDef("team-a", "root", ""),
		componentDef("team-a", "middle", "root"),
		componentDef("team-a", "leaf", "middle"),
	)
	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
		Spec: v1beta1.ApplicationSpec{
			Components: []common.ApplicationComponent{{Name: "c", Type: "leaf"}},
		},
	}

	usage := collectDefinitionUsage(app)
	h.addInheritedTypes(context.Background(), app, usage)

	for _, name := range []string{"leaf", "middle", "root"} {
		require.Contains(t, usage.componentTypes, name)
	}
}

// A type that extends nothing, or one that cannot be resolved, adds nothing and
// does not fail: that is a different problem, reported elsewhere.
func TestPermissionCheckIsQuietWhenThereIsNothingToAdd(t *testing.T) {
	h := handlerWith(componentDef("team-a", "plain", ""))
	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
		Spec: v1beta1.ApplicationSpec{
			Components: []common.ApplicationComponent{
				{Name: "a", Type: "plain"},
				{Name: "b", Type: "does-not-exist"},
			},
		},
	}

	usage := collectDefinitionUsage(app)
	h.addInheritedTypes(context.Background(), app, usage)
	require.Len(t, usage.componentTypes, 2)
}

// A cycle in `extends` must not spin the check.
func TestPermissionCheckSurvivesACycle(t *testing.T) {
	h := handlerWith(
		componentDef("team-a", "a", "b"),
		componentDef("team-a", "b", "a"),
	)
	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
		Spec: v1beta1.ApplicationSpec{
			Components: []common.ApplicationComponent{{Name: "c", Type: "a"}},
		},
	}

	usage := collectDefinitionUsage(app)
	h.addInheritedTypes(context.Background(), app, usage)
	require.Contains(t, usage.componentTypes, "b")
}

// The standard install: definitions in vela-system, Application elsewhere.
func TestPermissionCheckReachesTheSystemNamespace(t *testing.T) {
	h := handlerWith(
		componentDef("vela-system", "webservice", ""),
		componentDef("vela-system", "custom-web", "webservice"),
	)
	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
		Spec: v1beta1.ApplicationSpec{
			Components: []common.ApplicationComponent{{Name: "c", Type: "custom-web"}},
		},
	}

	usage := collectDefinitionUsage(app)
	h.addInheritedTypes(context.Background(), app, usage)

	require.Contains(t, usage.componentTypes, "webservice",
		"a definition in vela-system extending another there must still be checked")
}

// A local definition shadowing a system one is the one walked.
func TestPermissionCheckPrefersTheAppNamespace(t *testing.T) {
	h := handlerWith(
		componentDef("team-a", "custom-web", "local-base"),
		componentDef("vela-system", "custom-web", "system-base"),
	)
	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team-a"},
		Spec: v1beta1.ApplicationSpec{
			Components: []common.ApplicationComponent{{Name: "c", Type: "custom-web"}},
		},
	}

	usage := collectDefinitionUsage(app)
	h.addInheritedTypes(context.Background(), app, usage)

	require.Contains(t, usage.componentTypes, "local-base")
	require.NotContains(t, usage.componentTypes, "system-base")
}
