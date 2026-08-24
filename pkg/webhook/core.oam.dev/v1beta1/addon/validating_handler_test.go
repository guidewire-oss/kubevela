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

package addon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

func rawProps(t *testing.T, m map[string]interface{}) *runtime.RawExtension {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return &runtime.RawExtension{Raw: b}
}

type fakeAdmissionDecoder struct {
	decodeFn func(req admission.Request, into runtime.Object) error
}

func (f fakeAdmissionDecoder) Decode(req admission.Request, into runtime.Object) error {
	return f.decodeFn(req, into)
}

func (f fakeAdmissionDecoder) DecodeRaw(_ runtime.RawExtension, _ runtime.Object) error {
	return errors.New("not implemented in test")
}

func TestValidateComponents(t *testing.T) {
	testCases := map[string]struct {
		components   []common.ApplicationComponent
		checker      func(calls *int) func(ctx context.Context, addon, version, registry string) *field.Error
		wantErrCount int
		wantField    string
		wantCalls    int
	}{
		"compatible addon component produces no error": {
			components: []common.ApplicationComponent{
				{Name: "fluxcd", Type: ComponentType, Properties: rawProps(t, map[string]interface{}{"addon": "fluxcd", "version": "1.0.0"})},
			},
			checker: func(calls *int) func(context.Context, string, string, string) *field.Error {
				return func(_ context.Context, _, _, _ string) *field.Error { *calls++; return nil }
			},
			wantErrCount: 0,
			wantCalls:    1,
		},
		"incompatible addon component yields one field error on the properties path": {
			components: []common.ApplicationComponent{
				{Name: "webservice-comp", Type: "webservice"},
				{Name: "fluxcd", Type: ComponentType, Properties: rawProps(t, map[string]interface{}{"addon": "fluxcd"})},
			},
			checker: func(calls *int) func(context.Context, string, string, string) *field.Error {
				return func(_ context.Context, _, _, _ string) *field.Error {
					*calls++
					return field.Invalid(field.NewPath("x"), "fluxcd", "requires kubernetes >= 1.30")
				}
			},
			wantErrCount: 1,
			wantField:    "spec.components[1].properties",
			wantCalls:    1,
		},
		"skipVersionValidate short-circuits before the checker": {
			components: []common.ApplicationComponent{
				{Name: "fluxcd", Type: ComponentType, Properties: rawProps(t, map[string]interface{}{"addon": "fluxcd", "skipVersionValidate": true})},
			},
			checker: func(calls *int) func(context.Context, string, string, string) *field.Error {
				return func(_ context.Context, _, _, _ string) *field.Error {
					*calls++
					return field.Invalid(field.NewPath("x"), "fluxcd", "should never be reached")
				}
			},
			wantErrCount: 0,
			wantCalls:    0,
		},
		"fail-open checker (registry error) yields no denial": {
			components: []common.ApplicationComponent{
				{Name: "fluxcd", Type: ComponentType, Properties: rawProps(t, map[string]interface{}{"addon": "fluxcd"})},
			},
			checker: func(calls *int) func(context.Context, string, string, string) *field.Error {
				return func(_ context.Context, _, _, _ string) *field.Error { *calls++; return nil }
			},
			wantErrCount: 0,
			wantCalls:    1,
		},
		"non-addon components are ignored": {
			components: []common.ApplicationComponent{
				{Name: "comp1", Type: "webservice"},
				{Name: "comp2", Type: "worker"},
			},
			checker: func(calls *int) func(context.Context, string, string, string) *field.Error {
				return func(_ context.Context, _, _, _ string) *field.Error {
					*calls++
					return field.Invalid(field.NewPath("x"), "x", "should never be reached")
				}
			},
			wantErrCount: 0,
			wantCalls:    0,
		},
		"addon name defaults to component name when addon field empty": {
			components: []common.ApplicationComponent{
				{Name: "velaux", Type: ComponentType},
			},
			checker: func(calls *int) func(context.Context, string, string, string) *field.Error {
				return func(_ context.Context, addon, _, _ string) *field.Error {
					*calls++
					if addon != "velaux" {
						return field.Invalid(field.NewPath("x"), addon, "unexpected addon name")
					}
					return nil
				}
			},
			wantErrCount: 0,
			wantCalls:    1,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			h := &ValidatingHandler{compatChecker: tc.checker(&calls)}
			app := &v1beta1.Application{Spec: v1beta1.ApplicationSpec{Components: tc.components}}

			errs := h.ValidateComponents(context.Background(), app)

			assert.Len(t, errs, tc.wantErrCount)
			assert.Equal(t, tc.wantCalls, calls, "unexpected checker invocation count")
			if tc.wantField != "" {
				require.Len(t, errs, 1)
				assert.Equal(t, tc.wantField, errs[0].Field)
			}
		})
	}
}

func TestHandle(t *testing.T) {
	newReq := func(op admissionv1.Operation) admission.Request {
		return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "req-1",
			Name:      "my-app",
			Namespace: "default",
			Operation: op,
		}}
	}

	t.Run("non-create-update operations are allowed", func(t *testing.T) {
		h := &ValidatingHandler{}
		resp := h.Handle(context.Background(), newReq(admissionv1.Delete))
		assert.True(t, resp.Allowed)
	})

	t.Run("decode error returns bad request", func(t *testing.T) {
		h := &ValidatingHandler{Decoder: fakeAdmissionDecoder{decodeFn: func(_ admission.Request, _ runtime.Object) error {
			return errors.New("bad payload")
		}}}
		resp := h.Handle(context.Background(), newReq(admissionv1.Create))
		assert.False(t, resp.Allowed)
		require.NotNil(t, resp.Result)
		assert.Equal(t, int32(http.StatusBadRequest), resp.Result.Code)
		assert.Contains(t, resp.Result.Message, "failed to decode")
	})

	t.Run("deleting application is allowed", func(t *testing.T) {
		h := &ValidatingHandler{Decoder: fakeAdmissionDecoder{decodeFn: func(_ admission.Request, into runtime.Object) error {
			app := into.(*v1beta1.Application)
			ts := metav1.NewTime(time.Now())
			app.ObjectMeta.DeletionTimestamp = &ts
			return nil
		}}}
		resp := h.Handle(context.Background(), newReq(admissionv1.Update))
		assert.True(t, resp.Allowed)
	})

	t.Run("validation errors are denied", func(t *testing.T) {
		h := &ValidatingHandler{
			Decoder: fakeAdmissionDecoder{decodeFn: func(_ admission.Request, into runtime.Object) error {
				app := into.(*v1beta1.Application)
				app.Spec.Components = []common.ApplicationComponent{{
					Name:       "fluxcd",
					Type:       ComponentType,
					Properties: rawProps(t, map[string]interface{}{"addon": "fluxcd"}),
				}}
				return nil
			}},
			compatChecker: func(_ context.Context, _, _, _ string) *field.Error {
				return field.Invalid(field.NewPath("x"), "fluxcd", "incompatible")
			},
		}
		resp := h.Handle(context.Background(), newReq(admissionv1.Create))
		assert.False(t, resp.Allowed)
		require.NotNil(t, resp.Result)
		assert.Equal(t, int32(http.StatusBadRequest), resp.Result.Code)
		assert.Contains(t, resp.Result.Message, "incompatible")
	})

	t.Run("valid request is allowed", func(t *testing.T) {
		h := &ValidatingHandler{
			Decoder: fakeAdmissionDecoder{decodeFn: func(_ admission.Request, into runtime.Object) error {
				app := into.(*v1beta1.Application)
				app.Spec.Components = []common.ApplicationComponent{{
					Name:       "fluxcd",
					Type:       ComponentType,
					Properties: rawProps(t, map[string]interface{}{"addon": "fluxcd"}),
				}}
				return nil
			}},
			compatChecker: func(_ context.Context, _, _, _ string) *field.Error {
				return nil
			},
		}
		resp := h.Handle(context.Background(), newReq(admissionv1.Update))
		assert.True(t, resp.Allowed)
	})
}

// TestWebhookPathMatchesChart guards the one drift that fails silently: if the route
// the handler registers and the path the ValidatingWebhookConfiguration points at
// disagree, the API server calls an unregistered path and the check never runs.
//
// It also asserts the webhook name appears in the caBundle-preservation lookup block.
// An entry missing from that block renders with the placeholder caBundle on every
// upgrade, which breaks admission rather than failing loudly at install.
func TestWebhookPathMatchesChart(t *testing.T) {
	const chart = "../../../../../charts/vela-core/templates/admission-webhooks/validatingWebhookConfiguration.yaml"

	raw, err := os.ReadFile(chart)
	require.NoError(t, err, "chart template must be readable from the test's working directory")
	body := string(raw)

	assert.Contains(t, body, "path: "+ValidationWebhookPath,
		"the chart must point at the route RegisterValidatingHandler serves")

	const webhookName = "validating.core.oam.dev.v1beta1.addoncomponents"
	assert.Contains(t, body, "name: "+webhookName)
	assert.Contains(t, body, `set $vals "addoncomponents"`,
		"the webhook name must be in the caBundle-preservation lookup, or upgrades render a placeholder CA")

	assert.Contains(t, body, ".Values.featureGates.enableAddonComponent",
		"the entry must be gated so it is not installed when the feature is off")
}
