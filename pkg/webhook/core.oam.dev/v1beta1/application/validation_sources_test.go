package application

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

func TestValidateSources(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)

	baseDefs := []runtime.Object{
		&v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "source-a", Namespace: "default"},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &common.Schematic{
					CUE: &common.CUE{
						Template: `
schema: {
  nested: {
    image: string
  }
}
output: {
  nested: image: parameter.image
}
parameter: {
  image: string
}
`,
					},
				},
			},
		},
		&v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "source-b", Namespace: "default"},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &common.Schematic{
					CUE: &common.CUE{
						Template: `
schema: {
  rendered: {
    image: string
  }
}
output: {
  rendered: image: parameter.image
}
parameter: {
  image: string
}
`,
					},
				},
			},
		},
	}

	tests := []struct {
		name          string
		app           *v1beta1.Application
		expectedErrs  int
		expectedField string
	}{
		{
			name: "valid references and source ordering",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{
							Name:       "clusterInfo",
							Type:       "source-a",
							Properties: rawJSON(`{"image":"nginx:1.25.0"}`),
						},
						{
							Name: "rendered",
							Type: "source-b",
							Properties: rawJSON(`{
  "image":{"fromSource":"clusterInfo.nested.image"}
}`),
						},
					},
					Components: []common.ApplicationComponent{
						{
							Name:       "web",
							Type:       "webservice",
							Properties: rawJSON(`{"image":{"fromSource":"rendered.rendered.image"}}`),
						},
					},
				},
			},
			expectedErrs: 0,
		},
		{
			name: "reject unknown source name in component",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "clusterInfo", Type: "source-a", Properties: rawJSON(`{"image":"nginx:1.25.0"}`)},
					},
					Components: []common.ApplicationComponent{
						{
							Name:       "web",
							Type:       "webservice",
							Properties: rawJSON(`{"image":{"fromSource":"missing.nested.image"}}`),
						},
					},
				},
			},
			expectedErrs:  1,
			expectedField: "spec.components[0].properties.image.fromSource",
		},
		{
			name: "reject unknown schema path",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "clusterInfo", Type: "source-a", Properties: rawJSON(`{"image":"nginx:1.25.0"}`)},
					},
					Components: []common.ApplicationComponent{
						{
							Name:       "web",
							Type:       "webservice",
							Properties: rawJSON(`{"image":{"fromSource":"clusterInfo.nested.tag"}}`),
						},
					},
				},
			},
			expectedErrs:  1,
			expectedField: "spec.components[0].properties.image.fromSource",
		},
		{
			name: "reject forward source dependency",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{
							Name: "first",
							Type: "source-b",
							Properties: rawJSON(`{
  "image":{"fromSource":"second.nested.image"}
}`),
						},
						{Name: "second", Type: "source-a", Properties: rawJSON(`{"image":"nginx:1.25.0"}`)},
					},
				},
			},
			expectedErrs:  1,
			expectedField: "spec.sources[0].properties.image.fromSource",
		},
		{
			name: "reject duplicate source names",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "dup", Type: "source-a", Properties: rawJSON(`{"image":"nginx:1.25.0"}`)},
						{Name: "dup", Type: "source-b", Properties: rawJSON(`{"image":"nginx:1.25.0"}`)},
					},
				},
			},
			expectedErrs:  1,
			expectedField: "spec.sources[1].name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(baseDefs...).Build()
			handler := &ValidatingHandler{Client: client}
			errs := handler.ValidateSources(context.Background(), tt.app)
			assert.Len(t, errs, tt.expectedErrs)
			if tt.expectedField != "" && len(errs) > 0 {
				assert.Equal(t, tt.expectedField, errs[0].Field)
			}
		})
	}
}

func rawJSON(s string) *runtime.RawExtension {
	return &runtime.RawExtension{Raw: []byte(s)}
}
