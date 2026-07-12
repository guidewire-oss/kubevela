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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"

	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/cue/definition"
	"github.com/oam-dev/kubevela/pkg/oam"
)

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

func TestResolvedSourceHash(t *testing.T) {
	t.Run("no source context -> not consumed", func(t *testing.T) {
		_, ok := resolvedSourceHash(&appfile.Component{Name: "web"})
		assert.False(t, ok)
	})

	t.Run("statuses present but nothing consumed -> not consumed", func(t *testing.T) {
		comp := compWithConsumed(map[string]map[string]interface{}{"rng": {}})
		_, ok := resolvedSourceHash(comp)
		assert.False(t, ok)
	})

	t.Run("consumed values produce a stable hash", func(t *testing.T) {
		comp := compWithConsumed(map[string]map[string]interface{}{"rng": {"value": 3}})
		h1, ok := resolvedSourceHash(comp)
		assert.True(t, ok)
		assert.NotEmpty(t, h1)
		// Same input -> same hash.
		h2, _ := resolvedSourceHash(compWithConsumed(map[string]map[string]interface{}{"rng": {"value": 3}}))
		assert.Equal(t, h1, h2)
	})

	t.Run("different resolved value -> different hash", func(t *testing.T) {
		h3, _ := resolvedSourceHash(compWithConsumed(map[string]map[string]interface{}{"rng": {"value": 3}}))
		h4, _ := resolvedSourceHash(compWithConsumed(map[string]map[string]interface{}{"rng": {"value": 4}}))
		assert.NotEqual(t, h3, h4, "a re-resolved value must change the hash")
	})
}

func TestStampResolvedSourceHash(t *testing.T) {
	wl := &unstructured.Unstructured{}
	wl.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	stampResolvedSourceHash(wl, "abc123")
	assert.Equal(t, "abc123", wl.GetAnnotations()[oam.AnnotationSourceResolvedHash])

	// nil / empty hash is a no-op (no panic, no annotation).
	stampResolvedSourceHash(nil, "x")
	wl2 := &unstructured.Unstructured{}
	stampResolvedSourceHash(wl2, "")
	assert.Empty(t, wl2.GetAnnotations()[oam.AnnotationSourceResolvedHash])
}

func TestLiveResolvedSourceHash(t *testing.T) {
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	desired.SetNamespace("default")
	desired.SetName("web")

	t.Run("absent workload -> empty (treated as changed)", func(t *testing.T) {
		cli := fake.NewClientBuilder().Build()
		assert.Equal(t, "", liveResolvedSourceHash(context.Background(), cli, "", desired))
	})

	t.Run("reads the stamped annotation from the live object", func(t *testing.T) {
		live := desired.DeepCopy()
		live.SetAnnotations(map[string]string{oam.AnnotationSourceResolvedHash: "deadbeef"})
		cli := fake.NewClientBuilder().WithObjects(live).Build()
		assert.Equal(t, "deadbeef", liveResolvedSourceHash(context.Background(), cli, "", desired))
	})
}
