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

package sourcedefinition

import (
	"context"
	"fmt"
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/logging"
	webhookutils "github.com/oam-dev/kubevela/pkg/webhook/utils"
)

var sourceDefGVR = v1beta1.SourceDefinitionGVR

// ValidatingHandler handles validation of SourceDefinition.
type ValidatingHandler struct {
	Client client.Client
	// Decoder decodes objects
	Decoder admission.Decoder
}

var _ admission.Handler = &ValidatingHandler{}

// Handle validates a SourceDefinition on create and update.
func (h *ValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	startTime := time.Now()
	ctx = logging.WithRequestID(ctx, string(req.UID))
	logger := logging.NewHandlerLogger(ctx, req, "SourceDefinitionValidator")

	logger.WithStep("start").Info("Starting admission validation for SourceDefinition resource", "operation", req.Operation, "resourceVersion", req.Kind.Version)

	if req.Resource.String() != sourceDefGVR.String() {
		err := fmt.Errorf("expect resource to be %s", sourceDefGVR)
		logger.WithStep("resource-check").WithError(err).Error(err, "Admission request targets unexpected resource type - rejecting request",
			"expected", sourceDefGVR.String(),
			"actual", req.Resource.String(),
			"operation", req.Operation)
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("%s (requestUID=%s)", err.Error(), req.UID))
	}

	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		logger.WithStep("skip-validation").Info("Skipping SourceDefinition validation - operation does not require validation", "operation", req.Operation, "reason", "only CREATE and UPDATE operations are validated")
		return admission.ValidationResponse(true, "")
	}

	obj := &v1beta1.SourceDefinition{}
	if err := h.Decoder.Decode(req, obj); err != nil {
		logger.WithStep("decode").WithError(err).Error(err, "Unable to decode admission request payload into SourceDefinition object - malformed request")
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("%s (requestUID=%s)", err.Error(), req.UID))
	}

	if obj.Spec.Schematic == nil || obj.Spec.Schematic.CUE == nil {
		err := fmt.Errorf("SourceDefinition must declare spec.schematic.cue")
		logger.WithStep("validate-schematic").WithError(err).Error(err, "SourceDefinition has no CUE schematic - resolution logic is required")
		return admission.Denied(fmt.Sprintf("%s (requestUID=%s)", err.Error(), req.UID))
	}
	cueTemplate := obj.Spec.Schematic.CUE.Template

	logger.WithStep("validate-cue").Info("Validating CUE template syntax and semantics for SourceDefinition schematic")
	if err := webhookutils.ValidateCuexTemplate(ctx, cueTemplate); err != nil {
		logger.WithStep("validate-cue").WithError(err).Error(err, "CUE template contains syntax errors or invalid constructs - template compilation failed")
		return admission.Denied(fmt.Sprintf("%s (requestUID=%s)", err.Error(), req.UID))
	}

	// A SourceDefinition without a storage key has no deterministic cache
	// identity; reject it here rather than synthesising one at resolution time.
	if err := ValidateSourceStorage(cueTemplate); err != nil {
		logger.WithStep("validate-storage").WithError(err).Error(err, "SourceDefinition storage block is invalid - a cache key is required")
		return admission.Denied(fmt.Sprintf("%s (requestUID=%s)", err.Error(), req.UID))
	}

	// Without a schema: block, both the admission path check and the runtime
	// output check are skipped, leaving fromSource unvalidated in either layer.
	if err := ValidateSourceSchema(cueTemplate); err != nil {
		logger.WithStep("validate-schema").WithError(err).Error(err, "SourceDefinition schema block is invalid - a schema is required to validate fromSource paths")
		return admission.Denied(fmt.Sprintf("%s (requestUID=%s)", err.Error(), req.UID))
	}

	logger.WithStep("complete").WithSuccess(true, startTime).Info("SourceDefinition admission validation completed successfully - resource is valid and will be admitted", "definitionName", obj.Name, "operation", req.Operation)
	return admission.ValidationResponse(true, "")
}

// RegisterValidatingHandler registers SourceDefinition validation with the webhook server.
func RegisterValidatingHandler(mgr manager.Manager) {
	server := mgr.GetWebhookServer()
	server.Register("/validating-core-oam-dev-v1beta1-sourcedefinitions", &webhook.Admission{Handler: &ValidatingHandler{
		Client:  mgr.GetClient(),
		Decoder: admission.NewDecoder(mgr.GetScheme()),
	}})
}
