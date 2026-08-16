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

// Package spokecluster holds the pure validation and defaulting rules for the
// SpokeCluster CR, shared by the validating and mutating admission handlers.
package spokecluster

import (
	"fmt"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/multicluster"
)

const defaultSecretKey = "kubeconfig"

// Validate checks a SpokeCluster against the Phase 1 policy rules that the
// structural schema cannot express: connect-only mode, the reserved cluster
// name, the credential union's exactly-one-arm and per-provider required
// fields, same-namespace kubeconfig secretRef, and rejection of
// infraProvisioning in connect mode. blueprintRef and rolloutStrategyRef are
// accepted and ignored. It has no client or context dependency so it can run
// identically in the webhook and in tests.
func Validate(sc *v1beta1.SpokeCluster) field.ErrorList {
	var errs field.ErrorList

	if sc.Spec.Mode != v1beta1.SpokeClusterModeConnect {
		errs = append(errs, field.Invalid(field.NewPath("spec", "mode"), sc.Spec.Mode,
			"mode must be 'connect' (provision and adopt are not supported in Phase 1)"))
	}

	if sc.Name == multicluster.ClusterLocalName {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), sc.Name,
			"name must not be the reserved local cluster name"))
	}

	errs = append(errs, validateCredential(sc.Namespace, sc.Spec.Credential)...)

	// infraProvisioning belongs to mode: provision. blueprintRef and
	// rolloutStrategyRef stay accepted and ignored so GitOps can land the
	// Phase 2 shape early; no Phase 1 controller reads them.
	if sc.Spec.InfraProvisioning != nil {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "infraProvisioning"),
			"infraProvisioning is not supported in connect mode (Phase 2)"))
	}

	return errs
}

// validateCredential enforces the discriminated union: exactly the arm named
// by type is set (every other arm is forbidden), plus the per-provider required
// fields. spokeNamespace is the SpokeCluster's own namespace and is used to
// reject cross-namespace kubeconfig secretRef (confused-deputy Secret reads).
func validateCredential(spokeNamespace string, cred v1beta1.CredentialSpec) field.ErrorList {
	credPath := field.NewPath("spec", "credential")
	var errs field.ErrorList

	// Forbid every arm that does not match the selected type, so a stored spec
	// never carries a stray arm (for example an azure arm under type kubeconfig).
	arms := []struct {
		name string
		set  bool
	}{
		{"kubeconfig", cred.Kubeconfig != nil},
		{"aws", cred.AWS != nil},
		{"azure", cred.Azure != nil},
		{"gcp", cred.GCP != nil},
	}
	for _, arm := range arms {
		if arm.set && arm.name != string(cred.Type) {
			errs = append(errs, field.Forbidden(credPath.Child(arm.name),
				fmt.Sprintf("%s must not be set when type is '%s'", arm.name, cred.Type)))
		}
	}

	switch cred.Type {
	case v1beta1.CredentialTypeKubeconfig:
		if cred.Kubeconfig == nil {
			errs = append(errs, field.Required(credPath.Child("kubeconfig"), "kubeconfig is required when type is 'kubeconfig'"))
		} else {
			errs = append(errs, validateKubeconfigCredential(spokeNamespace, credPath.Child("kubeconfig"), cred.Kubeconfig)...)
		}

	case v1beta1.CredentialTypeAWS:
		if cred.AWS == nil {
			errs = append(errs, field.Required(credPath.Child("aws"), "aws is required when type is 'aws'"))
		} else {
			errs = append(errs, validateAWSCredential(credPath.Child("aws"), cred.AWS)...)
		}

	default:
		errs = append(errs, field.NotSupported(credPath.Child("type"), cred.Type,
			[]string{string(v1beta1.CredentialTypeKubeconfig), string(v1beta1.CredentialTypeAWS)}))
	}

	return errs
}

// validateKubeconfigCredential checks the kubeconfig arm's required fields and
// the same-namespace secretRef policy. An empty secretRef.namespace is fine:
// Materialize falls back to the SpokeCluster's namespace. An explicit value that
// differs lets any principal who can create a SpokeCluster coerce the
// controller's cluster-wide Secret read into an exfil of another namespace's
// credential, so it is forbidden here.
func validateKubeconfigCredential(spokeNamespace string, kubePath *field.Path, kc *v1beta1.KubeconfigCredential) field.ErrorList {
	var errs field.ErrorList
	if kc.SecretRef.Name == "" {
		errs = append(errs, field.Required(kubePath.Child("secretRef", "name"), "secretRef.name is required"))
	}
	if ns := kc.SecretRef.Namespace; ns != "" && ns != spokeNamespace {
		errs = append(errs, field.Forbidden(kubePath.Child("secretRef", "namespace"),
			fmt.Sprintf("cross-namespace secretRef is not permitted (SpokeCluster is in %q, secretRef.namespace is %q); omit namespace to use the SpokeCluster's namespace", spokeNamespace, ns)))
	}
	return errs
}

// validateAWSCredential validates the aws credential arm's required fields
// and the authMode enum.
func validateAWSCredential(awsPath *field.Path, aws *v1beta1.AWSCredential) field.ErrorList {
	var errs field.ErrorList

	if aws.AuthMode != v1beta1.AWSAuthModePodIdentity && aws.AuthMode != v1beta1.AWSAuthModeIRSA {
		errs = append(errs, field.NotSupported(awsPath.Child("authMode"), aws.AuthMode,
			[]string{string(v1beta1.AWSAuthModePodIdentity), string(v1beta1.AWSAuthModeIRSA)}))
	}
	if aws.ClusterName == "" {
		errs = append(errs, field.Required(awsPath.Child("clusterName"), "clusterName is required"))
	}
	if aws.Region == "" {
		errs = append(errs, field.Required(awsPath.Child("region"), "region is required"))
	}
	if aws.RoleARN == "" {
		errs = append(errs, field.Required(awsPath.Child("roleArn"), "roleArn is required"))
	}
	// Require ExternalID whenever RoleARN is present so a stolen SpokeCluster
	// (or a confused-deputy caller) cannot AssumeRole without the shared secret
	// the trust policy should also demand (confused-deputy mitigation).
	if aws.RoleARN != "" && aws.ExternalID == "" {
		errs = append(errs, field.Required(awsPath.Child("externalId"),
			"externalId is required with roleArn (set the same value in the role trust policy Condition)"))
	}

	return errs
}

// Default applies the one default the CRD schema cannot express:
// kubeconfig.secretRef.key. mode, the probe intervals, and deletionPolicy all
// carry +kubebuilder:default markers, so the apiserver already fills them when
// they are absent. Re-setting them here would only diverge from schema-only
// admission by overwriting an explicit (invalid) zero value and masking it, so
// they are deliberately left to the schema.
func Default(sc *v1beta1.SpokeCluster) {
	// secretRef.namespace is intentionally left untouched: the fallback to the
	// SpokeCluster's own namespace happens at credential resolve time, not here.
	if sc.Spec.Credential.Type == v1beta1.CredentialTypeKubeconfig && sc.Spec.Credential.Kubeconfig != nil {
		if sc.Spec.Credential.Kubeconfig.SecretRef.Key == "" {
			sc.Spec.Credential.Kubeconfig.SecretRef.Key = defaultSecretKey
		}
	}
}
