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

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/cue/definition"
	"github.com/oam-dev/kubevela/pkg/oam"
)

var cmGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

func compWithConsumed(consumed map[string]map[string]interface{}) *appfile.Component {
	comp := &appfile.Component{Name: "web"}
	comp.Ctx = appfile.NewBasicContext(velaprocess.ContextData{
		Namespace: "default",
		AppName:   "app",
		CompName:  "web",
	}, nil)
	if consumed != nil {
		statuses := map[string]definition.SourceResolutionStatus{}
		for name, fields := range consumed {
			statuses[name] = definition.SourceResolutionStatus{Name: name, ConsumedFields: fields}
		}
		comp.Ctx.PushData(definition.SourceResolutionStatusKey, statuses)
	}
	return comp
}

func TestResolvedSourceHashes(t *testing.T) {
	t.Run("no source context -> not consumed", func(t *testing.T) {
		_, ok := resolvedSourceHashes(&appfile.Component{Name: "web"})
		assert.False(t, ok)
	})

	t.Run("statuses present but nothing consumed -> not consumed", func(t *testing.T) {
		_, ok := resolvedSourceHashes(compWithConsumed(map[string]map[string]interface{}{"rng": {}}))
		assert.False(t, ok)
	})

	t.Run("per-source hashes are stable and value-sensitive", func(t *testing.T) {
		h1, ok := resolvedSourceHashes(compWithConsumed(map[string]map[string]interface{}{
			"rng": {"value": 3}, "tenant": {"name": "acme"},
		}))
		assert.True(t, ok)
		assert.Len(t, h1, 2)

		// Same inputs -> identical per-source hashes.
		h2, _ := resolvedSourceHashes(compWithConsumed(map[string]map[string]interface{}{
			"rng": {"value": 3}, "tenant": {"name": "acme"},
		}))
		assert.Equal(t, h1, h2)

		// Change only rng -> only rng's hash changes.
		h3, _ := resolvedSourceHashes(compWithConsumed(map[string]map[string]interface{}{
			"rng": {"value": 4}, "tenant": {"name": "acme"},
		}))
		assert.NotEqual(t, h1["rng"], h3["rng"])
		assert.Equal(t, h1["tenant"], h3["tenant"])
	})
}

func TestChangedSources(t *testing.T) {
	current := map[string]string{"rng": "a", "tenant": "b"}
	assert.ElementsMatch(t, []string{}, changedSources(current, map[string]string{"rng": "a", "tenant": "b"}))
	assert.ElementsMatch(t, []string{"rng"}, changedSources(current, map[string]string{"rng": "x", "tenant": "b"}))
	// Missing from live (first apply / new source) counts as changed.
	assert.ElementsMatch(t, []string{"rng", "tenant"}, changedSources(current, nil))
}

func TestSourceAutoUpdateSelector(t *testing.T) {
	all := func(m map[string]string) bool { a, _, s := sourceAutoUpdateSelector(m); return a && s.enabled(false) }
	off := func(m map[string]string) bool { _, _, s := sourceAutoUpdateSelector(m); return !s.enabled(false) }

	assert.True(t, off(map[string]string{}), "no annotations -> disabled")
	assert.True(t, all(map[string]string{oam.AnnotationAutoUpdate: "true"}), "autoUpdate enables all")
	assert.True(t, all(map[string]string{oam.AnnotationAutoUpdateSources: "true"}))
	assert.True(t, all(map[string]string{oam.AnnotationAutoUpdateSources: "*"}))
	assert.True(t, off(map[string]string{oam.AnnotationAutoUpdateSources: ""}), "empty -> disabled")
	assert.True(t, off(map[string]string{oam.AnnotationAutoUpdateSources: " , "}), "only separators -> disabled")

	matchAll, set, listed := sourceAutoUpdateSelector(map[string]string{oam.AnnotationAutoUpdateSources: "rng, tenant"})
	assert.Equal(t, autoUpdateOn, listed)
	assert.False(t, matchAll)
	assert.Contains(t, set, "rng")
	assert.Contains(t, set, "tenant")
	assert.NotContains(t, set, "cluster")
}

func TestStampAndLiveResolvedSourceHashes(t *testing.T) {
	wl := &unstructured.Unstructured{}
	wl.SetGroupVersionKind(cmGVK)
	wl.SetNamespace("default")
	wl.SetName("web")
	stampResolvedSourceHashes(wl, map[string]string{"rng": "aaa", "tenant": "bbb"})
	assert.NotEmpty(t, wl.GetAnnotations()[oam.AnnotationSourceResolvedHash])

	// no-op cases
	stampResolvedSourceHashes(nil, map[string]string{"x": "y"})
	wl2 := &unstructured.Unstructured{}
	stampResolvedSourceHashes(wl2, nil)
	assert.Empty(t, wl2.GetAnnotations()[oam.AnnotationSourceResolvedHash])

	t.Run("live read round-trips the stamped map", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithObjects(wl.DeepCopy()).Build()
		desired := &unstructured.Unstructured{}
		desired.SetGroupVersionKind(cmGVK)
		desired.SetNamespace("default")
		desired.SetName("web")
		got := liveResolvedSourceHashes(context.Background(), cli, "", desired)
		assert.Equal(t, map[string]string{"rng": "aaa", "tenant": "bbb"}, got)
	})

	t.Run("absent workload -> nil (everything counts as changed)", func(t *testing.T) {
		cli := fake.NewClientBuilder().Build()
		desired := &unstructured.Unstructured{}
		desired.SetGroupVersionKind(cmGVK)
		desired.SetNamespace("default")
		desired.SetName("missing")
		assert.Nil(t, liveResolvedSourceHashes(context.Background(), cli, "", desired))
	})
}

func TestSourceAutoUpdateSelectorOptOut(t *testing.T) {
	off := func(m map[string]string) bool { _, _, s := sourceAutoUpdateSelector(m); return !s.enabled(false) }
	state := func(m map[string]string) autoUpdateState { _, _, s := sourceAutoUpdateSelector(m); return s }

	// An explicit "no" must not be read as a source binding named "false".
	assert.True(t, off(map[string]string{oam.AnnotationAutoUpdateSources: "false"}))
	assert.True(t, off(map[string]string{oam.AnnotationAutoUpdateSources: "off"}))
	assert.True(t, off(map[string]string{oam.AnnotationAutoUpdateSources: "none"}))

	// The narrower annotation wins over the broad one.
	assert.True(t, off(map[string]string{
		oam.AnnotationAutoUpdate:        "true",
		oam.AnnotationAutoUpdateSources: "false",
	}))

	// Saying nothing is distinct from saying no, so a controller-wide default
	// has something to apply to.
	assert.Equal(t, autoUpdateUnset, state(map[string]string{}))
	assert.Equal(t, autoUpdateOff, state(map[string]string{oam.AnnotationAutoUpdateSources: "False"}))
	assert.Equal(t, autoUpdateOff, state(map[string]string{oam.AnnotationAutoUpdateSources: " OFF "}))
	assert.Equal(t, autoUpdateOn, state(map[string]string{oam.AnnotationAutoUpdate: "true"}))

	assert.False(t, autoUpdateUnset.enabled(false))
	assert.True(t, autoUpdateUnset.enabled(true), "silence defers to the controller default")
	assert.True(t, autoUpdateOn.enabled(false), "an explicit yes overrides a default of off")
	assert.False(t, autoUpdateOff.enabled(true), "an explicit no overrides a default of on")
}

func TestSourceRefreshEnabled(t *testing.T) {
	app := func(annotations map[string]string) *v1beta1.Application {
		return &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default", Annotations: annotations},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{{Name: "registry", Type: "configmap"}},
			},
		}
	}
	optedIn := map[string]string{oam.AnnotationAutoUpdateSources: "true"}

	ok, _ := sourceRefreshEnabled(app(optedIn), false)
	assert.True(t, ok, "opted in with sources declared")

	ok, reason := sourceRefreshEnabled(app(nil), false)
	assert.False(t, ok)
	assert.Contains(t, reason, "opted in")

	// A publishVersion pin is hard. A source value changing must not move what
	// is deployed until the user bumps the pin, matching the valuesFrom
	// fingerprint gate in workflow.go.
	pinned := map[string]string{
		oam.AnnotationAutoUpdateSources: "true",
		oam.AnnotationPublishVersion:    "v1",
	}
	ok, reason = sourceRefreshEnabled(app(pinned), false)
	assert.False(t, ok, "publishVersion suppresses source refresh")
	assert.Contains(t, reason, "publishVersion")

	// The controller-wide default reaches an Application with no opinion, but
	// the pin still wins over it.
	ok, _ = sourceRefreshEnabled(app(nil), true)
	assert.True(t, ok, "default on reaches an Application with no opinion")
	ok, _ = sourceRefreshEnabled(app(map[string]string{oam.AnnotationPublishVersion: "v1"}), true)
	assert.False(t, ok, "the pin beats a default of on")
}
