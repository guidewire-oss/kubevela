package application

import (
	"context"
	"strings"
	"testing"

	wfTypesv1alpha1 "github.com/kubevela/pkg/apis/oam/v1alpha1"
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
		// The optional-field / default rule is target-aware and covered in
		// TestValidateFromSourceTargetTypes, which supplies real target
		// definitions with required vs optional parameters.
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

// TestValidateFromSourceTargetTypes covers the target contract: a fromSource
// output field type must be compatible with the consuming component/trait
// parameter type.
func TestValidateFromSourceTargetTypes(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)

	srcStr := &v1beta1.SourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "str-source", Namespace: "default"},
		Spec: v1beta1.SourceDefinitionSpec{Schematic: &common.Schematic{CUE: &common.CUE{Template: `
schema: {
  image:  string
  count:  int
  vpcId?: string
}
output: {
  image: "nginx"
  count: 3
}
parameter: {}
`}}},
	}
	// webservice declares a required "image" and an optional "note", used to
	// exercise the KEP rule: a default is required only when an optional source
	// field feeds a required target.
	compDef := &v1beta1.ComponentDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webservice", Namespace: "default"},
		Spec: v1beta1.ComponentDefinitionSpec{Schematic: &common.Schematic{CUE: &common.CUE{Template: `
output: {}
parameter: {
  image:    string
  replicas: int
  note?:    string
}
`}}},
	}
	traitDef := &v1beta1.TraitDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "scaler", Namespace: "default"},
		Spec: v1beta1.TraitDefinitionSpec{Schematic: &common.Schematic{CUE: &common.CUE{Template: `
outputs: {}
parameter: {
  replicas: int
}
`}}},
	}
	defs := []runtime.Object{srcStr, compDef, traitDef}

	tests := []struct {
		name         string
		app          *v1beta1.Application
		expectedErrs int
	}{
		{
			name: "compatible: string schema into string param",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{{Name: "s", Type: "str-source", Properties: rawJSON(`{}`)}},
					Components: []common.ApplicationComponent{{
						Name: "web", Type: "webservice",
						Properties: rawJSON(`{"image":{"fromSource":"s.image"}}`),
					}},
				},
			},
			expectedErrs: 0,
		},
		{
			name: "incompatible: string schema into int component param",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{{Name: "s", Type: "str-source", Properties: rawJSON(`{}`)}},
					Components: []common.ApplicationComponent{{
						Name: "web", Type: "webservice",
						Properties: rawJSON(`{"replicas":{"fromSource":"s.image"}}`),
					}},
				},
			},
			expectedErrs: 1,
		},
		{
			name: "compatible: int schema into int trait param",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{{Name: "s", Type: "str-source", Properties: rawJSON(`{}`)}},
					Components: []common.ApplicationComponent{{
						Name: "web", Type: "webservice",
						Properties: rawJSON(`{"image":"nginx"}`),
						Traits: []common.ApplicationTrait{{
							Type:       "scaler",
							Properties: rawJSON(`{"replicas":{"fromSource":"s.count"}}`),
						}},
					}},
				},
			},
			expectedErrs: 0,
		},
		{
			name: "incompatible: string schema into int trait param",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{{Name: "s", Type: "str-source", Properties: rawJSON(`{}`)}},
					Components: []common.ApplicationComponent{{
						Name: "web", Type: "webservice",
						Properties: rawJSON(`{"image":"nginx"}`),
						Traits: []common.ApplicationTrait{{
							Type:       "scaler",
							Properties: rawJSON(`{"replicas":{"fromSource":"s.image"}}`),
						}},
					}},
				},
			},
			expectedErrs: 1,
		},
		// KEP default rule: required only when an optional source field feeds a
		// required target.
		{
			name: "reject: optional source field into REQUIRED target without default",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{{Name: "s", Type: "str-source", Properties: rawJSON(`{}`)}},
					Components: []common.ApplicationComponent{{
						Name: "web", Type: "webservice",
						// vpcId? (optional) -> image (required), no default
						Properties: rawJSON(`{"image":{"fromSource":"s.vpcId"}}`),
					}},
				},
			},
			expectedErrs: 1,
		},
		{
			name: "accept: optional source field into REQUIRED target WITH default",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{{Name: "s", Type: "str-source", Properties: rawJSON(`{}`)}},
					Components: []common.ApplicationComponent{{
						Name: "web", Type: "webservice",
						Properties: rawJSON(`{"image":{"fromSource":{"name":"s","path":"vpcId","default":"none"}}}`),
					}},
				},
			},
			expectedErrs: 0,
		},
		{
			name: "accept: optional source field into OPTIONAL target without default",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{{Name: "s", Type: "str-source", Properties: rawJSON(`{}`)}},
					Components: []common.ApplicationComponent{{
						Name: "web", Type: "webservice",
						// vpcId? (optional) -> note? (optional): no default needed
						Properties: rawJSON(`{"note":{"fromSource":"s.vpcId"}}`),
					}},
				},
			},
			expectedErrs: 0,
		},
		{
			name: "accept: REQUIRED source field into REQUIRED target without default",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{{Name: "s", Type: "str-source", Properties: rawJSON(`{}`)}},
					Components: []common.ApplicationComponent{{
						Name: "web", Type: "webservice",
						// image (required) -> image (required): no default needed
						Properties: rawJSON(`{"image":{"fromSource":"s.image"}}`),
					}},
				},
			},
			expectedErrs: 0,
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

// fromSource is substituted during component and trait rendering only. Anywhere
// else the consumer would be handed the literal {"fromSource": ...} map, so the
// directive is rejected rather than admitted into a silent no-op.
func TestValidateSourcesRejectsUnsupportedSurfaces(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)

	def := &v1beta1.SourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "source-a", Namespace: "default"},
		Spec: v1beta1.SourceDefinitionSpec{
			Schematic: &common.Schematic{CUE: &common.CUE{Template: `
schema: {image: string}
storage: {key: "source-a"}
output: {image: parameter.image}
parameter: {image: string}
`}},
		},
	}

	source := []v1beta1.ApplicationSource{
		{Name: "img", Type: "source-a", Properties: rawJSON(`{"image":"nginx:1.25.0"}`)},
	}
	validComp := []common.ApplicationComponent{
		{Name: "web", Type: "webservice", Properties: rawJSON(`{"image":"nginx"}`)},
	}

	tests := []struct {
		name    string
		app     *v1beta1.Application
		wantMsg string
	}{
		{
			name: "policy properties",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources:    source,
					Components: validComp,
					Policies: []v1beta1.AppPolicy{
						{Name: "p", Type: "override", Properties: rawJSON(`{"image":{"fromSource":"img.image"}}`)},
					},
				},
			},
			wantMsg: "not supported in policy properties",
		},
		{
			name: "workflow step properties",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources:    source,
					Components: validComp,
					Workflow: &v1beta1.Workflow{
						Steps: []wfTypesv1alpha1.WorkflowStep{
							{
								WorkflowStepBase: wfTypesv1alpha1.WorkflowStepBase{
									Name:       "notify",
									Type:       "notification",
									Properties: rawJSON(`{"url":{"fromSource":"img.image"}}`),
								},
							},
						},
					},
				},
			},
			wantMsg: "not supported in workflow step properties",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &ValidatingHandler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(def).Build()}
			errs := h.ValidateSources(context.Background(), tc.app)
			if len(errs) == 0 {
				t.Fatalf("expected a rejection mentioning %q, got none", tc.wantMsg)
			}
			var joined string
			for _, e := range errs {
				joined += e.Error() + "\n"
			}
			if !strings.Contains(joined, tc.wantMsg) {
				t.Fatalf("expected an error containing %q, got:\n%s", tc.wantMsg, joined)
			}
		})
	}
}

// A SourceDefinition can restrict which surfaces may consume it.
func TestValidateSourcesHonoursConsumableFrom(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)

	componentOnly := &v1beta1.SourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "component-only", Namespace: "default"},
		Spec: v1beta1.SourceDefinitionSpec{
			Schematic: &common.Schematic{CUE: &common.CUE{Template: `
consumableFrom: ["component"]
schema: {image: string}
storage: {key: "component-only"}
output: {image: parameter.image}
parameter: {image: string}
`}},
		},
	}

	source := []v1beta1.ApplicationSource{
		{Name: "img", Type: "component-only", Properties: rawJSON(`{"image":"nginx:1.25.0"}`)},
	}

	t.Run("allowed from a component", func(t *testing.T) {
		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: v1beta1.ApplicationSpec{
				Sources: source,
				Components: []common.ApplicationComponent{
					{Name: "web", Type: "webservice", Properties: rawJSON(`{"image":{"fromSource":"img.image"}}`)},
				},
			},
		}
		h := &ValidatingHandler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(componentOnly).Build()}
		if errs := h.ValidateSources(context.Background(), app); len(errs) != 0 {
			t.Fatalf("expected no errors, got: %v", errs)
		}
	})

	t.Run("rejected from a trait", func(t *testing.T) {
		app := &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: v1beta1.ApplicationSpec{
				Sources: source,
				Components: []common.ApplicationComponent{
					{
						Name: "web", Type: "webservice", Properties: rawJSON(`{"image":"nginx"}`),
						Traits: []common.ApplicationTrait{
							{Type: "scaler", Properties: rawJSON(`{"image":{"fromSource":"img.image"}}`)},
						},
					},
				},
			},
		}
		h := &ValidatingHandler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(componentOnly).Build()}
		errs := h.ValidateSources(context.Background(), app)
		if len(errs) == 0 {
			t.Fatal("expected the trait binding to be rejected")
		}
		var joined string
		for _, e := range errs {
			joined += e.Error() + "\n"
		}
		if !strings.Contains(joined, "cannot be consumed from a trait") {
			t.Fatalf("expected a consumableFrom rejection, got:\n%s", joined)
		}
	})
}
