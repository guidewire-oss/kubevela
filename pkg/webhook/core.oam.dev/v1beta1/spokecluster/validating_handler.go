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

package spokecluster

import (
	"context"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

var _ admission.Handler = &ValidatingHandler{}

// ValidatingHandler validates SpokeCluster resources on Create and Update.
// Client is an uncached reader (typically mgr.GetAPIReader()) so uniqueness
// checks see SpokeClusters that were just admitted and are not yet in the
// informer cache.
type ValidatingHandler struct {
	Client  client.Reader
	Decoder admission.Decoder
}

// Handle validates the SpokeCluster carried by the admission request.
func (h *ValidatingHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Resource.String() != v1beta1.SpokeClusterGVR.String() {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("expect resource to be %s", v1beta1.SpokeClusterGVR))
	}

	if req.Operation == admissionv1.Create || req.Operation == admissionv1.Update {
		sc := &v1beta1.SpokeCluster{}
		if err := h.Decoder.Decode(req, sc); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if errs := Validate(sc); len(errs) > 0 {
			return admission.Denied(errs.ToAggregate().Error())
		}
		if errs := h.validateClusterScopedUniqueness(ctx, sc); len(errs) > 0 {
			return admission.Denied(errs.ToAggregate().Error())
		}
	}

	return admission.ValidationResponse(true, "")
}

// validateClusterScopedUniqueness enforces identity constraints that span
// namespaces: SpokeCluster metadata.name (gateway Secret key) and AWS roleArn
// (one hub AssumeRole target per spoke).
func (h *ValidatingHandler) validateClusterScopedUniqueness(ctx context.Context, sc *v1beta1.SpokeCluster) field.ErrorList {
	if h.Client == nil {
		return nil
	}
	list := &v1beta1.SpokeClusterList{}
	if err := h.Client.List(ctx, list); err != nil {
		return field.ErrorList{field.InternalError(field.NewPath("metadata", "name"),
			fmt.Errorf("failed to list SpokeClusters for uniqueness checks: %w", err))}
	}
	if errs := checkUniqueName(sc, list); len(errs) > 0 {
		return errs
	}
	return checkUniqueAWSRoleARN(sc, list)
}

func checkUniqueName(sc *v1beta1.SpokeCluster, list *v1beta1.SpokeClusterList) field.ErrorList {
	for i := range list.Items {
		other := &list.Items[i]
		if other.Name != sc.Name {
			continue
		}
		if other.Namespace == sc.Namespace {
			continue
		}
		return field.ErrorList{field.Duplicate(field.NewPath("metadata", "name"),
			fmt.Sprintf("SpokeCluster name %q is already used by %s/%s; names must be unique cluster-wide because the gateway Secret is keyed by name alone",
				sc.Name, other.Namespace, other.Name))}
	}
	return nil
}

func checkUniqueAWSRoleARN(sc *v1beta1.SpokeCluster, list *v1beta1.SpokeClusterList) field.ErrorList {
	if sc.Spec.Credential.Type != v1beta1.CredentialTypeAWS || sc.Spec.Credential.AWS == nil {
		return nil
	}
	roleARN := sc.Spec.Credential.AWS.RoleARN
	if roleARN == "" {
		return nil
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Namespace == sc.Namespace && other.Name == sc.Name {
			continue
		}
		if other.Spec.Credential.Type != v1beta1.CredentialTypeAWS || other.Spec.Credential.AWS == nil {
			continue
		}
		if other.Spec.Credential.AWS.RoleARN != roleARN {
			continue
		}
		return field.ErrorList{field.Duplicate(field.NewPath("spec", "credential", "aws", "roleArn"),
			fmt.Sprintf("roleArn %q is already used by SpokeCluster %s/%s; each AWS spoke must use a distinct role so a tenant cannot share another spoke's AssumeRole target",
				roleARN, other.Namespace, other.Name))}
	}
	return nil
}

// RegisterValidatingHandler registers the SpokeCluster validating webhook on
// the given manager's webhook server.
func RegisterValidatingHandler(mgr manager.Manager) {
	mgr.GetWebhookServer().Register("/validating-core-oam-dev-v1beta1-spokeclusters", &webhook.Admission{
		Handler: &ValidatingHandler{
			Client:  mgr.GetAPIReader(),
			Decoder: admission.NewDecoder(mgr.GetScheme()),
		},
	})
}
