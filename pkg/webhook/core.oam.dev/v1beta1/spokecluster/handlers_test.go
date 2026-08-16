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
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

// spokeClusterGVR mirrors v1beta1.SpokeClusterGVR as the metav1 type used on
// admission.Request.Resource.
var spokeClusterGVR = metav1.GroupVersionResource{
	Group:    v1beta1.SpokeClusterGVR.Group,
	Version:  v1beta1.SpokeClusterGVR.Version,
	Resource: v1beta1.SpokeClusterGVR.Resource,
}

// newSpokeClusterRequest marshals sc into an admission.Request with the given
// operation and the spokeclusters GVR, as the apiserver would send it.
func newSpokeClusterRequest(sc *v1beta1.SpokeCluster, op admissionv1.Operation) admission.Request {
	raw, err := json.Marshal(sc)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Resource:  spokeClusterGVR,
		Operation: op,
		Object:    runtime.RawExtension{Raw: raw},
	}}
}

// patchValue looks up a jsonpatch operation by path (add or replace, either
// is a valid outcome depending on whether the field was present in the raw
// request) and returns its value.
func patchValue(patches []jsonpatch.Operation, path string) interface{} {
	for _, p := range patches {
		if p.Path == path {
			return p.Value
		}
	}
	Fail(fmt.Sprintf("no patch found for path %s (patches: %+v)", path, patches))
	return nil
}

var _ = Describe("ValidatingHandler", func() {
	var handler *ValidatingHandler

	BeforeEach(func() {
		handler = &ValidatingHandler{Decoder: decoder, Client: fake.NewClientBuilder().WithScheme(testScheme).Build()}
	})

	It("allows a valid kubeconfig spoke on create", func() {
		req := newSpokeClusterRequest(validKubeconfigSpoke(), admissionv1.Create)
		resp := handler.Handle(context.Background(), req)
		gomega.Expect(resp.Allowed).To(gomega.BeTrue(), "response: %+v", resp.Result)
	})

	It("denies provision mode on create", func() {
		sc := validKubeconfigSpoke()
		sc.Spec.Mode = v1beta1.SpokeClusterModeProvision
		req := newSpokeClusterRequest(sc, admissionv1.Create)
		resp := handler.Handle(context.Background(), req)
		gomega.Expect(resp.Allowed).To(gomega.BeFalse())
	})

	It("bad-requests a mismatched resource GVR", func() {
		req := newSpokeClusterRequest(validKubeconfigSpoke(), admissionv1.Create)
		req.Resource = metav1.GroupVersionResource{Group: "core.oam.dev", Version: "v1beta1", Resource: "applications"}
		resp := handler.Handle(context.Background(), req)
		gomega.Expect(resp.Allowed).To(gomega.BeFalse())
	})

	It("admits delete without decoding", func() {
		req := newSpokeClusterRequest(validKubeconfigSpoke(), admissionv1.Delete)
		resp := handler.Handle(context.Background(), req)
		gomega.Expect(resp.Allowed).To(gomega.BeTrue(), "response: %+v", resp.Result)
	})

	It("denies a create that reuses another namespace's SpokeCluster name", func() {
		existing := validKubeconfigSpoke()
		existing.Namespace = "team-a"
		existing.Name = "shared-name"
		cli := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(existing).Build()
		handler = &ValidatingHandler{Decoder: decoder, Client: cli}

		incoming := validKubeconfigSpoke()
		incoming.Namespace = "team-b"
		incoming.Name = "shared-name"
		req := newSpokeClusterRequest(incoming, admissionv1.Create)
		resp := handler.Handle(context.Background(), req)
		gomega.Expect(resp.Allowed).To(gomega.BeFalse())
		gomega.Expect(resp.Result.Message).To(gomega.ContainSubstring("unique cluster-wide"))
	})

	It("allows update of an existing SpokeCluster in its own namespace", func() {
		existing := validKubeconfigSpoke()
		cli := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(existing).Build()
		handler = &ValidatingHandler{Decoder: decoder, Client: cli}

		req := newSpokeClusterRequest(existing, admissionv1.Update)
		resp := handler.Handle(context.Background(), req)
		gomega.Expect(resp.Allowed).To(gomega.BeTrue(), "response: %+v", resp.Result)
	})

	It("denies an AWS create that reuses another spoke's roleArn", func() {
		existing := validAWSSpoke()
		existing.Name = "spoke-a"
		cli := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(existing).Build()
		handler = &ValidatingHandler{Decoder: decoder, Client: cli}

		incoming := validAWSSpoke()
		incoming.Name = "spoke-b"
		incoming.Spec.Credential.AWS.ClusterName = "spoke-b"
		req := newSpokeClusterRequest(incoming, admissionv1.Create)
		resp := handler.Handle(context.Background(), req)
		gomega.Expect(resp.Allowed).To(gomega.BeFalse())
		gomega.Expect(resp.Result.Message).To(gomega.ContainSubstring("roleArn"))
	})

	It("denies an AWS create that reuses another namespace's roleArn", func() {
		existing := validAWSSpoke()
		existing.Namespace = "team-a"
		existing.Name = "spoke-a"
		cli := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(existing).Build()
		handler = &ValidatingHandler{Decoder: decoder, Client: cli}

		incoming := validAWSSpoke()
		incoming.Namespace = "team-b"
		incoming.Name = "spoke-b"
		incoming.Spec.Credential.AWS.ClusterName = "spoke-b"
		req := newSpokeClusterRequest(incoming, admissionv1.Create)
		resp := handler.Handle(context.Background(), req)
		gomega.Expect(resp.Allowed).To(gomega.BeFalse())
		gomega.Expect(resp.Result.Message).To(gomega.ContainSubstring("roleArn"))
	})

	It("allows an AWS create with a distinct roleArn", func() {
		existing := validAWSSpoke()
		existing.Name = "spoke-a"
		cli := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(existing).Build()
		handler = &ValidatingHandler{Decoder: decoder, Client: cli}

		incoming := validAWSSpoke()
		incoming.Name = "spoke-b"
		incoming.Spec.Credential.AWS.ClusterName = "spoke-b"
		incoming.Spec.Credential.AWS.RoleARN = "arn:aws:iam::123456789012:role/other-role"
		req := newSpokeClusterRequest(incoming, admissionv1.Create)
		resp := handler.Handle(context.Background(), req)
		gomega.Expect(resp.Allowed).To(gomega.BeTrue(), "response: %+v", resp.Result)
	})
})

var _ = Describe("MutatingHandler", func() {
	It("defaults secretRef.key and returns it as a JSON patch", func() {
		handler := &MutatingHandler{Decoder: decoder}

		sc := validKubeconfigSpoke()
		sc.Spec.Credential.Kubeconfig.SecretRef.Key = ""

		req := newSpokeClusterRequest(sc, admissionv1.Create)
		resp := handler.Handle(context.Background(), req)

		gomega.Expect(resp.Allowed).To(gomega.BeTrue(), "response: %+v", resp.Result)
		gomega.Expect(patchValue(resp.Patches, "/spec/credential/kubeconfig/secretRef/key")).To(gomega.Equal(defaultSecretKey))

		// mode, probe knobs, and deletionPolicy are left to the CRD schema
		// defaults, so the mutating webhook must not patch them.
		for _, p := range resp.Patches {
			gomega.Expect(p.Path).NotTo(gomega.BeElementOf(
				"/spec/mode", "/spec/probeIntervalSeconds", "/spec/probeTimeoutSeconds", "/spec/deletionPolicy"))
		}
	})

	It("preserves an explicitly set zero-value field instead of stripping it", func() {
		handler := &MutatingHandler{Decoder: decoder}

		// probeIntervalSeconds: 0 is explicitly set in the raw request. It is
		// invalid (schema minimum is 10) and must reach schema validation intact,
		// not be silently stripped by the mutating webhook (which would let schema
		// defaulting mask it). Built as raw JSON because the struct's omitempty tag
		// drops the zero before it could reach the handler.
		raw := []byte(`{"apiVersion":"core.oam.dev/v1beta1","kind":"SpokeCluster",` +
			`"metadata":{"name":"sc","namespace":"default"},` +
			`"spec":{"probeIntervalSeconds":0,"credential":{"type":"kubeconfig",` +
			`"kubeconfig":{"secretRef":{"name":"n"}}}}}`)
		req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Resource:  spokeClusterGVR,
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: raw},
		}}

		resp := handler.Handle(context.Background(), req)

		gomega.Expect(resp.Allowed).To(gomega.BeTrue(), "response: %+v", resp.Result)
		// The key is still defaulted...
		gomega.Expect(patchValue(resp.Patches, "/spec/credential/kubeconfig/secretRef/key")).To(gomega.Equal(defaultSecretKey))
		// ...but probeIntervalSeconds is never touched, so the explicit 0 survives
		// for the apiserver's schema minimum to reject.
		for _, p := range resp.Patches {
			gomega.Expect(p.Path).NotTo(gomega.Equal("/spec/probeIntervalSeconds"),
				"mutating webhook must not strip an explicitly set probeIntervalSeconds")
		}
	})
})
