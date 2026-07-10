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
		&v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "source-opt", Namespace: "default"},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &common.Schematic{
					CUE: &common.CUE{
						Template: `
schema: {
  region:  string
  vpcId?:  string
}
output: {
  region: parameter.region
}
parameter: {
  region: string
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
			name: "reject optional schema field consumed without default",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "clusterInfo", Type: "source-opt", Properties: rawJSON(`{"region":"us-east-1"}`)},
					},
					Components: []common.ApplicationComponent{
						{
							Name:       "web",
							Type:       "webservice",
							Properties: rawJSON(`{"vpcId":{"fromSource":"clusterInfo.vpcId"}}`),
						},
					},
				},
			},
			expectedErrs:  1,
			expectedField: "spec.components[0].properties.vpcId.fromSource",
		},
		{
			name: "accept optional schema field consumed with default",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "clusterInfo", Type: "source-opt", Properties: rawJSON(`{"region":"us-east-1"}`)},
					},
					Components: []common.ApplicationComponent{
						{
							Name:       "web",
							Type:       "webservice",
							Properties: rawJSON(`{"vpcId":{"fromSource":{"name":"clusterInfo","path":"vpcId","default":""}}}`),
						},
					},
				},
			},
			expectedErrs: 0,
		},
		{
			name: "accept required schema field without default",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "clusterInfo", Type: "source-opt", Properties: rawJSON(`{"region":"us-east-1"}`)},
					},
					Components: []common.ApplicationComponent{
						{
							Name:       "web",
							Type:       "webservice",
							Properties: rawJSON(`{"region":{"fromSource":"clusterInfo.region"}}`),
						},
					},
				},
			},
			expectedErrs: 0,
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

// TestValidateSourceInputs covers the input contract: source properties must
// conform to the referenced SourceDefinition's parameter: block.
func TestValidateSourceInputs(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)

	defs := []runtime.Object{
		// param: image is string; also an int field for type tests
		&v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "typed-source", Namespace: "default"},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &common.Schematic{CUE: &common.CUE{Template: `
schema: {
  image: string
}
output: {
  image: parameter.image
}
parameter: {
  image:    string
  replicas: int
}
`}},
			},
		},
		// upstream source whose schema output "region" is a string
		&v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "region-source", Namespace: "default"},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &common.Schematic{CUE: &common.CUE{Template: `
schema: {
  region: string
}
output: {
  region: "us-east-1"
}
parameter: {}
`}},
			},
		},
		// source that declares no parameter block
		&v1beta1.SourceDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "noparam-source", Namespace: "default"},
			Spec: v1beta1.SourceDefinitionSpec{
				Schematic: &common.Schematic{CUE: &common.CUE{Template: `
schema: {
  value: string
}
output: {
  value: "x"
}
`}},
			},
		},
	}

	tests := []struct {
		name         string
		app          *v1beta1.Application
		expectedErrs int
	}{
		{
			name: "valid literal properties",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "s", Type: "typed-source", Properties: rawJSON(`{"image":"nginx:1.25","replicas":3}`)},
					},
				},
			},
			expectedErrs: 0,
		},
		{
			name: "reject unknown property field",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "s", Type: "typed-source", Properties: rawJSON(`{"image":"nginx","bogus":"x"}`)},
					},
				},
			},
			expectedErrs: 1,
		},
		{
			name: "reject type mismatch: string into int param",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "s", Type: "typed-source", Properties: rawJSON(`{"image":"nginx","replicas":"three"}`)},
					},
				},
			},
			expectedErrs: 1,
		},
		{
			name: "valid fromSource-fed property type",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "up", Type: "region-source", Properties: rawJSON(`{}`)},
						{Name: "s", Type: "typed-source", Properties: rawJSON(`{"image":{"fromSource":"up.region"},"replicas":1}`)},
					},
				},
			},
			expectedErrs: 0,
		},
		{
			name: "reject fromSource-fed type mismatch: string schema into int param",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "up", Type: "region-source", Properties: rawJSON(`{}`)},
						{Name: "s", Type: "typed-source", Properties: rawJSON(`{"image":"nginx","replicas":{"fromSource":"up.region"}}`)},
					},
				},
			},
			expectedErrs: 1,
		},
		{
			name: "reject property supplied to parameterless definition",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "s", Type: "noparam-source", Properties: rawJSON(`{"unexpected":"x"}`)},
					},
				},
			},
			expectedErrs: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(defs...).Build()
			handler := &ValidatingHandler{Client: cli}
			errs := handler.ValidateSources(context.Background(), tt.app)
			assert.Len(t, errs, tt.expectedErrs, "errors: %v", errs)
		})
	}
}
