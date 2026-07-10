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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
)

// sourceTemplateRefsByType must read ConfigTemplateRef from the LIVE
// SourceDefinition, because the appfile's RelatedSourceDefinitions have their
// Status wiped during parsing.
func TestSourceTemplateRefsByType(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NoError(t, v1beta1.AddToScheme(scheme))

	liveWithRef := &v1beta1.SourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "with-ref", Namespace: "vela-system"},
		Status: v1beta1.SourceDefinitionStatus{
			ConfigTemplateRef: &v1beta1.SourceDefinitionConfigTemplateRef{
				Name:       "source-with-ref-abcd1234",
				SchemaHash: "abcd1234",
			},
		},
	}
	liveInAppNS := &v1beta1.SourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "app-ns-ref", Namespace: "my-app-ns"},
		Status: v1beta1.SourceDefinitionStatus{
			ConfigTemplateRef: &v1beta1.SourceDefinitionConfigTemplateRef{Name: "source-app-ns-ref-0f0f"},
		},
	}
	liveNoRef := &v1beta1.SourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "no-ref", Namespace: "vela-system"},
	}

	cli := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(liveWithRef, liveInAppNS, liveNoRef).Build()

	// The appfile copies have wiped status, mirroring parseSources behaviour.
	af := &appfile.Appfile{
		Namespace: "my-app-ns",
		RelatedSourceDefinitions: map[string]*v1beta1.SourceDefinition{
			"with-ref":   {ObjectMeta: metav1.ObjectMeta{Name: "with-ref"}},
			"app-ns-ref": {ObjectMeta: metav1.ObjectMeta{Name: "app-ns-ref"}},
			"no-ref":     {ObjectMeta: metav1.ObjectMeta{Name: "no-ref"}},
			"missing":    {ObjectMeta: metav1.ObjectMeta{Name: "missing"}},
		},
	}

	got := sourceTemplateRefsByType(context.Background(), cli, af)

	assert.Equal(t, "source-with-ref-abcd1234", got["with-ref"], "should resolve ref from system namespace")
	assert.Equal(t, "source-app-ns-ref-0f0f", got["app-ns-ref"], "should resolve ref from app namespace")
	_, hasNoRef := got["no-ref"]
	assert.False(t, hasNoRef, "definition without a ConfigTemplateRef must be skipped")
	_, hasMissing := got["missing"]
	assert.False(t, hasMissing, "definition not found in cluster must be skipped")
}

func TestSourceTemplateRefsByTypeNilInputs(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NoError(t, v1beta1.AddToScheme(scheme))
	cli := fake.NewClientBuilder().WithScheme(scheme).Build()

	assert.Empty(t, sourceTemplateRefsByType(context.Background(), cli, nil))
	assert.Empty(t, sourceTemplateRefsByType(context.Background(), nil, &appfile.Appfile{}))
}
