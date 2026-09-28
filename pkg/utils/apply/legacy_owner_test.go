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

package apply

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// An existing resource carrying only marks from before owner labels: its owner has not
// re-applied it yet.
func unmigrated() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	obj.SetNamespace("default")
	obj.SetName("settings")
	obj.SetResourceVersion("1")
	obj.SetLabels(map[string]string{"legacy-owner": "shop"})
	return obj
}

// ownerLabelsOnly is a kind that reads owner labels only, as resourcetracker.Base does.
type ownerLabelsOnly struct{ key string }

func (o ownerLabelsOnly) Kind() string                          { return "Component" }
func (o ownerLabelsOnly) Key() string                           { return o.key }
func (o ownerLabelsOnly) ControlledBy(obj client.Object) string { return "" }

func legacyShop(obj client.Object) string {
	if obj.GetLabels()["legacy-owner"] == "shop" {
		return "Application/default/shop"
	}
	return ""
}

func TestLegacyControlledBy(t *testing.T) {
	prev := CurrentConfig()
	defer Configure(prev)

	check := func(key string, takeOver bool) error {
		return MustBeControlledBy(ownerLabelsOnly{key})(&applyAction{takeOver: takeOver}, unmigrated(), unmigrated())
	}

	Configure(Config{DefaultOwnerKind: "Application"})
	require.NoError(t, check("Component/default/backend", true), "no hook: nobody owns it, so take-over adopts it")

	Configure(Config{DefaultOwnerKind: "Application", LegacyControlledBy: legacyShop})
	require.ErrorContains(t, check("Component/default/backend", true), "managed by other component Application/default/shop",
		"with the hook it is the Application's, and take-over does not apply")
	require.ErrorContains(t, check("Component/default/backend", false), "Application/default/shop")
	require.NoError(t, check("default/shop", false), "the Application itself, keyed without a kind, still owns it")

	shared := func(key string) error {
		desired := unmigrated()
		return SharedBy(ownerLabelsOnly{key})(&applyAction{}, unmigrated(), desired)
	}
	require.ErrorContains(t, shared("Component/default/backend"), "controlled by Application/default/shop but is not sharable",
		"sharing asks the legacy owner too")
	require.NoError(t, shared("default/shop"))
}
