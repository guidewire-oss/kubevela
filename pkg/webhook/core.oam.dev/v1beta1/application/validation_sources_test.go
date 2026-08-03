package application

import (
	"context"
	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/definition/sourceexpr"
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
  "image":"$(source.clusterInfo.nested.image)"
}`),
						},
					},
					Components: []common.ApplicationComponent{
						{
							Name:       "web",
							Type:       "webservice",
							Properties: rawJSON(`{"image":"$(source.rendered.rendered.image)"}`),
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
							Properties: rawJSON(`{"image":"$(source.missing.nested.image)"}`),
						},
					},
				},
			},
			expectedErrs:  1,
			expectedField: "spec.components[0].properties.image",
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
							Properties: rawJSON(`{"image":"$(source.clusterInfo.nested.tag)"}`),
						},
					},
				},
			},
			expectedErrs:  1,
			expectedField: "spec.components[0].properties.image",
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
  "image":"$(source.second.nested.image)"
}`),
						},
						{Name: "second", Type: "source-a", Properties: rawJSON(`{"image":"nginx:1.25.0"}`)},
					},
				},
			},
			expectedErrs:  1,
			expectedField: "spec.sources[0].properties.image",
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
			name: "valid expression-fed property type",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "up", Type: "region-source", Properties: rawJSON(`{}`)},
						{Name: "s", Type: "typed-source", Properties: rawJSON(`{"image":"$(source.up.region)","replicas":1}`)},
					},
				},
			},
			expectedErrs: 0,
		},
		{
			name: "reject expression-fed type mismatch: string schema into int param",
			app: &v1beta1.Application{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
				Spec: v1beta1.ApplicationSpec{
					Sources: []v1beta1.ApplicationSource{
						{Name: "up", Type: "region-source", Properties: rawJSON(`{}`)},
						{Name: "s", Type: "typed-source", Properties: rawJSON(`{"image":"nginx","replicas":"$(source.up.region)"}`)},
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

// TestValidateExpressionTargetTypes covers the target contract: an expression
// output field type must be compatible with the consuming component/trait
// parameter type.
func TestValidateExpressionTargetTypes(t *testing.T) {
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
						Properties: rawJSON(`{"image":"$(source.s.image)"}`),
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
						Properties: rawJSON(`{"replicas":"$(source.s.image)"}`),
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
							Properties: rawJSON(`{"replicas":"$(source.s.count)"}`),
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
							Properties: rawJSON(`{"replicas":"$(source.s.image)"}`),
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
						Properties: rawJSON(`{"image":"$(source.s.vpcId)"}`),
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
						Properties: rawJSON(`{"image":"$(*source.s.vpcId | \"none\")"}`),
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
						Properties: rawJSON(`{"note":"$(source.s.vpcId)"}`),
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
						Properties: rawJSON(`{"image":"$(source.s.image)"}`),
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

// A source resolves during component and trait rendering only. Anywhere else the
// read cannot be honoured, so the
// directive is rejected rather than admitted into a silent no-op.
func TestValidateSourcesRejectsUnsupportedSurfaces(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)

	def := &v1beta1.SourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "source-a", Namespace: "default"},
		Spec: v1beta1.SourceDefinitionSpec{
			Schematic: &common.Schematic{CUE: &common.CUE{Template: `
schema: {image: string}
$internal: {key: "source-a"}
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
						{Name: "p", Type: "override", Properties: rawJSON(`{"image":"$(source.img.image)"}`)},
					},
				},
			},
			wantMsg: `"source" cannot be read here`,
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
$internal: {key: "component-only"}
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
					{Name: "web", Type: "webservice", Properties: rawJSON(`{"image":"$(source.img.image)"}`)},
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
							{Type: "scaler", Properties: rawJSON(`{"image":"$(source.img.image)"}`)},
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

// An open list - [...string] - has no concrete element at any index, only an
// element type. Properties are flattened to dotted paths with indices, so
// items: ["a","b"] becomes items.0 and items.1, and looking those up in a schema
// declaring items: [...string] found nothing - which rejected a perfectly valid
// list-valued property as undeclared.
//
// This affected the directive form before expressions existed; it is not an
// expression-specific bug.
func TestCueStructLookupHandlesOpenLists(t *testing.T) {
	v := cuecontext.New().CompileString(`root: {
		open:  [...string]
		fixed: [string, int]
		nested: [...{name: string}]
	}`)
	c := &cueStruct{root: v.LookupPath(cue.ParsePath("root"))}

	for _, tc := range []struct {
		path string
		want cue.Kind
	}{
		{"open.0", cue.StringKind},
		{"open.7", cue.StringKind}, // any index, since the type is what matters
		{"fixed.0", cue.StringKind},
		{"fixed.1", cue.IntKind},
		{"nested.0.name", cue.StringKind},
	} {
		got, declared := c.kindAt(tc.path)
		if !declared {
			t.Errorf("%s should resolve through the list's element type", tc.path)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: expected %s, got %s", tc.path, tc.want, got)
		}
	}

	// A fixed list still reports out-of-range as undeclared, rather than
	// silently falling back to an element type it does not have.
	if _, declared := c.kindAt("fixed.5"); declared {
		t.Error("an index beyond a fixed list must not resolve")
	}
}

// An open map - headers?: [string]: string - declares no concrete field at any
// key, only a value type. Properties are flattened to dotted paths, so passing
// {headers: {Accept: "text/plain"}} looked up "headers.Accept" and found
// nothing, reporting a perfectly valid property as undeclared.
//
// Same shape as the open-list bug: a pattern constraint is not a field.
func TestCueStructLookupHandlesOpenMaps(t *testing.T) {
	v := cuecontext.New().CompileString(`root: {
		headers?: [string]: string
		counts:   [string]: int
		nested:   [string]: {enabled: bool}
		declared: {known: string}
	}`)
	c := &cueStruct{root: v.LookupPath(cue.ParsePath("root"))}

	for _, tc := range []struct {
		path string
		want cue.Kind
	}{
		{"headers.Accept", cue.StringKind},
		{"headers.X-Anything", cue.StringKind},
		{"counts.shards", cue.IntKind},
		{"nested.a.enabled", cue.BoolKind},
		{"declared.known", cue.StringKind},
	} {
		got, declared := c.kindAt(tc.path)
		if !declared {
			t.Errorf("%s should resolve through the map's value type", tc.path)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: expected %s, got %s", tc.path, tc.want, got)
		}
	}

	// A closed struct still rejects an unknown field, so the fallback does not
	// make everything permissive.
	if _, declared := c.kindAt("declared.unknown"); declared {
		t.Error("an undeclared field of a closed struct must not resolve")
	}
}

// A reference path is joined with dots for the schema lookup, which only works
// when every segment is a plain field name.
//
// Regression test: `labels["platform.io/team"]` joins to `labels.platform.io/team`,
// which no longer says where the key began, and the lookup rejected it as
// undeclared - breaking every domain-prefixed label key, which is the normal
// Kubernetes convention. The unit suite missed it; a cluster caught it.
func TestPathIsOpaque(t *testing.T) {
	for _, tc := range []struct {
		name     string
		segments []string
		opaque   bool
	}{
		{"plain field", []string{"region"}, false},
		{"nested fields", []string{"nested", "image", "repo"}, false},
		{"map key without a dot", []string{"labels", "team"}, false},
		{"map key with a dot", []string{"labels", "platform.io/team"}, true},
		{"annotation key with a dot", []string{"annotations", "kubectl.kubernetes.io/last-applied"}, true},
		{"list index", []string{"outputs", "0", "kind"}, true},
		{"index at the end", []string{"ports", "2"}, true},
		{"a field merely containing digits", []string{"addr2"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathIsOpaque(tc.segments); got != tc.opaque {
				t.Errorf("pathIsOpaque(%v) = %v, want %v", tc.segments, got, tc.opaque)
			}
		})
	}
}

// The schema lookup must follow the three shapes a source schema actually uses,
// not just plain nested fields.
//
// Removing the directive routed every read through this lookup for the first
// time, and it rejected all three as undeclared - open maps (a Config's
// `outputs`, a component's `traits`), open fields (`properties: _`), and keys
// with a dot in them. Each is an ordinary read that TypeOf checks properly.
func TestSourceSchemaLookupShapes(t *testing.T) {
	schema := `{
		region: string
		nested: {image: {repo: string}}
		labels: [string]: string
		traits: [string]: {healthy: bool}
		properties: _
	}`
	v := &sourceSchemaValidator{schema: cuecontext.New().CompileString(schema)}
	if v.schema.Err() != nil {
		t.Fatalf("schema: %v", v.schema.Err())
	}

	for _, tc := range []struct {
		path string
		want bool
		why  string
	}{
		{"region", true, "a plain field"},
		{"nested.image.repo", true, "nested fields"},
		{"labels.team", true, "a key of an open map"},
		{"traits.scaler.healthy", true, "through an open map into its element type"},
		{"properties.endpoint", true, "below an open field, which TypeOf judges instead"},
		{"properties.a.b.c", true, "however deep below an open field"},
		{"nope", false, "an undeclared field is still rejected"},
		{"nested.image.nope", false, "and so is one nested inside a declared struct"},
	} {
		if got := v.HasPath(tc.path); got != tc.want {
			t.Errorf("HasPath(%q) = %v, want %v (%s)", tc.path, got, tc.want, tc.why)
		}
	}
}

// The target parameter has to be readable without compiling the whole template.
//
// Regression test: loadTargetParameter compiled the entire definition, which
// needs every package it imports registered with the compiler in hand.
// WorkloadCompiler holds the workload providers, not vela/multicluster or
// vela/builtin - so every workflow-step definition failed to compile and the
// type check silently passed. A mismatch in a step surfaced as a Go unmarshal
// error naming a struct field instead.
func TestParameterBlockOnly(t *testing.T) {
	ctx := context.Background()

	t.Run("extracted without the imports the body needs", func(t *testing.T) {
		// Shaped like the deploy step: imports this compiler does not hold.
		tmpl := `
import (
	"vela/multicluster"
	"vela/builtin"
)
deploy: multicluster.#Deploy & {$params: {policies: parameter.policies}}
suspend: builtin.#Suspend & {$params: message: "\(context.stepName)"}
parameter: {
	auto: *true | bool
	policies: *[] | [...string]
	parallelism: *5 | int
}
`
		param, ok := parameterBlockOnly(ctx, tmpl)
		if !ok {
			t.Fatal("the parameter block should compile on its own")
		}
		kind, declared := param.kindAt("parallelism")
		if !declared || kind != cue.IntKind {
			t.Fatalf("parallelism should be int, got %v (declared=%v)", kind, declared)
		}
		if kind, declared := param.kindAt("auto"); !declared || kind != cue.BoolKind {
			t.Fatalf("auto should be bool, got %v (declared=%v)", kind, declared)
		}
	})

	t.Run("a parameter referencing a local definition still resolves", func(t *testing.T) {
		tmpl := `
import "vela/kube"
#Args: {name: string, replicas: int}
output: kube.#Apply & {$params: {}}
parameter: #Args
`
		param, ok := parameterBlockOnly(ctx, tmpl)
		if !ok {
			t.Fatal("a parameter aliased to a local definition should resolve")
		}
		if kind, declared := param.kindAt("replicas"); !declared || kind != cue.IntKind {
			t.Fatalf("replicas should be int, got %v (declared=%v)", kind, declared)
		}
	})

	t.Run("no parameter block reports not-found rather than guessing", func(t *testing.T) {
		if _, ok := parameterBlockOnly(ctx, `output: {a: 1}`); ok {
			t.Fatal("a template with no parameter block must not report one")
		}
	})
}

// A chained source resolves in its consumer's render, so the surfaces it must
// satisfy are its consumers', not its own.
//
// This is what makes the compatibility check correct rather than merely present:
// checking a chained source against "source" would ask whether it can resolve on
// a surface that has no context of its own, which is never the real question.
func TestEffectiveSurfaces(t *testing.T) {
	ref := func(name, surface string, idx int) sourceReference {
		return sourceReference{SourceName: name, Surface: surface, SourceIndex: idx}
	}

	t.Run("a directly consumed binding gets its own surfaces", func(t *testing.T) {
		got := effectiveSurfaces([]sourceReference{
			ref("a", "component", -1),
			ref("a", "workflowstep", -1),
		}, map[int]string{})
		if want := []string{"component", "workflowstep"}; !reflect.DeepEqual(got["a"], want) {
			t.Fatalf("got %v, want %v", got["a"], want)
		}
	})

	// b is consumed by a component; a is consumed only by b. So a resolves in a
	// component's context, inherited through the chain.
	t.Run("a chained binding inherits its consumer's surfaces", func(t *testing.T) {
		got := effectiveSurfaces([]sourceReference{
			ref("b", "component", -1),
			ref("a", "source", 1), // read inside spec.sources[1], which is b
		}, map[int]string{0: "a", 1: "b"})
		if want := []string{"component"}; !reflect.DeepEqual(got["a"], want) {
			t.Fatalf("a should inherit %v, got %v", want, got["a"])
		}
	})

	// Two consumers on different surfaces: the chained source must satisfy both.
	t.Run("surfaces accumulate across consumers", func(t *testing.T) {
		got := effectiveSurfaces([]sourceReference{
			ref("b", "component", -1),
			ref("b", "workflowstep", -1),
			ref("a", "source", 1),
		}, map[int]string{0: "a", 1: "b"})
		want := []string{"component", "workflowstep"}
		if !reflect.DeepEqual(got["a"], want) {
			t.Fatalf("a should satisfy both %v, got %v", want, got["a"])
		}
	})

	// Two hops: c is consumed by a trait, b by c, a by b.
	t.Run("inheritance propagates the length of the chain", func(t *testing.T) {
		got := effectiveSurfaces([]sourceReference{
			ref("c", "trait", -1),
			ref("b", "source", 2), // inside c
			ref("a", "source", 1), // inside b
		}, map[int]string{0: "a", 1: "b", 2: "c"})
		for _, name := range []string{"a", "b", "c"} {
			if want := []string{"trait"}; !reflect.DeepEqual(got[name], want) {
				t.Errorf("%s should be %v, got %v", name, want, got[name])
			}
		}
	})

	// A binding nothing consumes resolves nowhere, so it constrains nothing.
	t.Run("an unconsumed binding has no surfaces", func(t *testing.T) {
		got := effectiveSurfaces([]sourceReference{ref("a", "component", -1)}, map[int]string{})
		if len(got["unused"]) != 0 {
			t.Fatalf("expected none, got %v", got["unused"])
		}
	})
}

// A source's properties are evaluated in its consumer's context, so a context
// read there must exist on every surface that consumes the binding.
//
// This is the reachable half of surface compatibility. A SourceDefinition's own
// template may only read universal context, so it can be consumed anywhere - but
// an Application can feed a source *from* context, which is how a per-component
// source is written, and that binding then only works where the field exists.
//
// The failure it prevents was silent: consumed from a workflow step, the read had
// nothing to resolve against, the step's expressions were left unsubstituted, and
// the literal "$(source.own.label)" was written into the rendered ConfigMap while
// the Application reported running.
func TestValidateSourceContextReads(t *testing.T) {
	app := func(props string) *v1beta1.Application {
		return &v1beta1.Application{
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{{
					Name: "own", Type: "percomp",
					Properties: &runtime.RawExtension{Raw: []byte(props)},
				}},
			},
		}
	}

	t.Run("component-only context consumed from a component is fine", func(t *testing.T) {
		errs := validateSourceContextReads(
			app(`{"component":"$(context.componentName)"}`),
			map[string][]string{"own": {"component"}})
		if len(errs) != 0 {
			t.Fatalf("expected none, got %v", errs)
		}
	})

	t.Run("component-only context consumed from a workflow step is refused", func(t *testing.T) {
		errs := validateSourceContextReads(
			app(`{"component":"$(context.componentName)"}`),
			map[string][]string{"own": {"workflowstep"}})
		if len(errs) != 1 {
			t.Fatalf("expected one error, got %v", errs)
		}
		for _, want := range []string{"componentName", "workflow step", `"own"`} {
			if !strings.Contains(errs[0].Error(), want) {
				t.Errorf("message should name %q; got %v", want, errs[0])
			}
		}
	})

	// The same binding used from both: it must satisfy the stricter one.
	t.Run("consumed from two surfaces, one of which lacks the field", func(t *testing.T) {
		errs := validateSourceContextReads(
			app(`{"component":"$(context.componentName)"}`),
			map[string][]string{"own": {"component", "workflowstep"}})
		if len(errs) != 1 {
			t.Fatalf("expected the workflow step to be refused, got %v", errs)
		}
	})

	t.Run("universal context is fine everywhere", func(t *testing.T) {
		errs := validateSourceContextReads(
			app(`{"component":"$(context.appName)"}`),
			map[string][]string{"own": {"component", "trait", "workflowstep"}})
		if len(errs) != 0 {
			t.Fatalf("appName exists on every surface; got %v", errs)
		}
	})

	// A source read is somebody else's business - chaining order and schema
	// paths are checked by the reference loop.
	t.Run("source reads are left to the reference pass", func(t *testing.T) {
		errs := validateSourceContextReads(
			app(`{"component":"$(source.other.field)"}`),
			map[string][]string{"own": {"workflowstep"}})
		if len(errs) != 0 {
			t.Fatalf("expected none, got %v", errs)
		}
	})

	// An unconsumed binding resolves nowhere, so it constrains nothing.
	t.Run("an unconsumed binding is not judged", func(t *testing.T) {
		errs := validateSourceContextReads(
			app(`{"component":"$(context.componentName)"}`), map[string][]string{})
		if len(errs) != 0 {
			t.Fatalf("expected none, got %v", errs)
		}
	})
}

// Policies are two different things wearing one name, and the difference decides
// whether a source can be read.
//
// A built-in policy - topology, override, garbage-collect - has its properties
// read straight off the appfile by a provider. Nothing renders them, so there is
// no resolver and a `source` read would survive as literal text. A policy with a
// CUE template goes through the same engine a component does, so a source
// resolves there exactly as it would in a component.
//
// Before this distinction existed both were refused, which made a
// resource-rendering PolicyDefinition the one definition kind that could not use
// the feature despite the machinery already being wired for it.
func TestValidateSourcesDistinguishesPolicyKinds(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = v1beta1.AddToScheme(scheme)

	src := &v1beta1.SourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "source-a", Namespace: "default"},
		Spec: v1beta1.SourceDefinitionSpec{
			Schematic: &common.Schematic{CUE: &common.CUE{Template: `
schema: {image: string}
$internal: {key: "source-a"}
output: {image: parameter.image}
parameter: {image: string}
`}},
		},
	}
	// A PolicyDefinition with a CUE template - the rendered kind.
	rendered := &v1beta1.PolicyDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "image-pin", Namespace: "default"},
		Spec: v1beta1.PolicyDefinitionSpec{
			Schematic: &common.Schematic{CUE: &common.CUE{Template: `
output: {
	apiVersion: "v1"
	kind:       "ConfigMap"
	metadata: name: context.name
	data: image: parameter.image
}
parameter: {image: string}
`}},
		},
	}

	app := func(policyType, props string) *v1beta1.Application {
		return &v1beta1.Application{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
			Spec: v1beta1.ApplicationSpec{
				Sources: []v1beta1.ApplicationSource{
					{Name: "img", Type: "source-a", Properties: rawJSON(`{"image":"nginx:1.25.0"}`)},
				},
				Components: []common.ApplicationComponent{
					{Name: "web", Type: "webservice", Properties: rawJSON(`{"image":"nginx"}`)},
				},
				Policies: []v1beta1.AppPolicy{
					{Name: "p", Type: policyType, Properties: rawJSON(props)},
				},
			},
		}
	}

	for _, tc := range []struct {
		name       string
		policyType string
		props      string
		wantMsg    string
	}{
		{
			name:       "a rendered policy resolves a source",
			policyType: "image-pin",
			props:      `{"image":"$(source.img.image)"}`,
		},
		{
			name:       "a rendered policy reads context",
			policyType: "image-pin",
			props:      `{"image":"$(context.appName)"}`,
		},
		{
			// The refusal must survive: a built-in policy still has no render.
			name:       "a built-in policy still cannot",
			policyType: "override",
			props:      `{"image":"$(source.img.image)"}`,
			wantMsg:    `"source" cannot be read here`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &ValidatingHandler{Client: fake.NewClientBuilder().
				WithScheme(scheme).WithObjects(src, rendered).Build()}
			var joined string
			for _, e := range h.ValidateSources(context.Background(), app(tc.policyType, tc.props)) {
				joined += e.Error() + "\n"
			}
			switch {
			case tc.wantMsg == "" && joined != "":
				t.Fatalf("expected acceptance, got:\n%s", joined)
			case tc.wantMsg != "" && !strings.Contains(joined, tc.wantMsg):
				t.Fatalf("expected an error containing %q, got:\n%s", tc.wantMsg, joined)
			}
		})
	}
}

// The two policy surfaces must offer different context, or the distinction above
// is cosmetic.
//
// A rendered policy is rendered for a cluster, so it gets the delivery context a
// component does. A built-in policy is consumed before placement is decided and
// has none of it - reading context.cluster there is the read that would type-check
// at admission and be absent at render.
func TestPolicySurfacesOfferDifferentContext(t *testing.T) {
	for _, field := range []string{"cluster", "publishVersion", "workflowName"} {
		if !sourceexpr.RenderedPolicyContext.Offers(field) {
			t.Errorf("a rendered policy renders for a cluster, so it should offer context.%s", field)
		}
		if sourceexpr.PolicyContext.Offers(field) {
			t.Errorf("a built-in policy is consumed before placement, so context.%s cannot be offered", field)
		}
	}
	// Both are policies, so both know which policy they are.
	for _, field := range []string{"policyName", "policyType", "appName", "namespace"} {
		if !sourceexpr.RenderedPolicyContext.Offers(field) || !sourceexpr.PolicyContext.Offers(field) {
			t.Errorf("both policy surfaces should offer context.%s", field)
		}
	}
}
