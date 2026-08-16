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
	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/multicluster"
)

// validKubeconfigSpoke returns a minimal, fully valid SpokeCluster using the
// kubeconfig credential arm.
func validKubeconfigSpoke() *v1beta1.SpokeCluster {
	return &v1beta1.SpokeCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-us-east-1", Namespace: "vela-system"},
		Spec: v1beta1.SpokeClusterSpec{
			Mode: v1beta1.SpokeClusterModeConnect,
			Credential: v1beta1.CredentialSpec{
				Type: v1beta1.CredentialTypeKubeconfig,
				Kubeconfig: &v1beta1.KubeconfigCredential{
					SecretRef: v1beta1.SecretKeyRef{Name: "prod-us-east-1-kubeconfig"},
				},
			},
		},
	}
}

// validAWSSpoke returns a minimal, fully valid SpokeCluster using the AWS
// credential arm.
func validAWSSpoke() *v1beta1.SpokeCluster {
	return &v1beta1.SpokeCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-us-east-1", Namespace: "vela-system"},
		Spec: v1beta1.SpokeClusterSpec{
			Mode: v1beta1.SpokeClusterModeConnect,
			Credential: v1beta1.CredentialSpec{
				Type: v1beta1.CredentialTypeAWS,
				AWS: &v1beta1.AWSCredential{
					AuthMode:    v1beta1.AWSAuthModePodIdentity,
					ClusterName: "prod-us-east-1",
					Region:      "us-east-1",
					RoleARN:     "arn:aws:iam::123456789012:role/per-cluster-role",
					ExternalID:  "us-east-1/123456789012/hub/vela-system/vela-core-cluster-core",
				},
			},
		},
	}
}

var _ = Describe("Validate", func() {
	DescribeTable("accepts valid spokes",
		func(spoke func() *v1beta1.SpokeCluster) {
			errs := Validate(spoke())
			gomega.Expect(errs).To(gomega.BeEmpty())
		},
		Entry("valid kubeconfig spoke", validKubeconfigSpoke),
		Entry("valid aws spoke", validAWSSpoke),
		Entry("kubeconfig with explicit same-namespace secretRef", func() *v1beta1.SpokeCluster {
			sc := validKubeconfigSpoke()
			sc.Spec.Credential.Kubeconfig.SecretRef.Namespace = sc.Namespace
			return sc
		}),
		Entry("blueprintRef and rolloutStrategyRef are accepted and ignored", func() *v1beta1.SpokeCluster {
			sc := validKubeconfigSpoke()
			sc.Spec.BlueprintRef = &v1beta1.BlueprintReference{Name: "baseline-platform", Revision: "v1"}
			sc.Spec.RolloutStrategyRef = &v1beta1.BlueprintReference{Name: "canary-10pct", Revision: "v2"}
			return sc
		}),
	)

	DescribeTable("rejects invalid spokes",
		func(base func() *v1beta1.SpokeCluster, mutate func(*v1beta1.SpokeCluster), wantField string) {
			sc := base()
			mutate(sc)

			errs := Validate(sc)
			gomega.Expect(errs).NotTo(gomega.BeEmpty(), "expected a validation error")

			var fields []string
			for _, e := range errs {
				fields = append(fields, e.Field)
			}
			gomega.Expect(fields).To(gomega.ContainElement(wantField), "errors: %v", errs.ToAggregate())
		},
		Entry("provision mode", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Mode = v1beta1.SpokeClusterModeProvision
		}, "spec.mode"),
		Entry("adopt mode", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Mode = v1beta1.SpokeClusterModeAdopt
		}, "spec.mode"),
		Entry("reserved name local", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Name = multicluster.ClusterLocalName
		}, "metadata.name"),
		Entry("both credential arms set (kubeconfig type, aws arm also set)", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.AWS = &v1beta1.AWSCredential{
				AuthMode:    v1beta1.AWSAuthModePodIdentity,
				ClusterName: "prod-us-east-1",
				Region:      "us-east-1",
				RoleARN:     "arn:aws:iam::123456789012:role/per-cluster-role",
			}
		}, "spec.credential.aws"),
		Entry("kubeconfig arm missing", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.Kubeconfig = nil
		}, "spec.credential.kubeconfig"),
		Entry("kubeconfig without secretRef.name", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.Kubeconfig.SecretRef.Name = ""
		}, "spec.credential.kubeconfig.secretRef.name"),
		Entry("cross-namespace secretRef", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.Kubeconfig.SecretRef.Namespace = "other-ns"
		}, "spec.credential.kubeconfig.secretRef.namespace"),
		Entry("azure type", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential = v1beta1.CredentialSpec{Type: v1beta1.CredentialTypeAzure, Azure: &v1beta1.AzureCredential{}}
		}, "spec.credential.type"),
		Entry("gcp type", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential = v1beta1.CredentialSpec{Type: v1beta1.CredentialTypeGCP, GCP: &v1beta1.GCPCredential{}}
		}, "spec.credential.type"),
		Entry("unsupported type", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential = v1beta1.CredentialSpec{Type: "oracle"}
		}, "spec.credential.type"),
		Entry("aws arm missing", validAWSSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.AWS = nil
		}, "spec.credential.aws"),
		Entry("bad aws.authMode", validAWSSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.AWS.AuthMode = "sts-assume-role"
		}, "spec.credential.aws.authMode"),
		Entry("missing aws.clusterName", validAWSSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.AWS.ClusterName = ""
		}, "spec.credential.aws.clusterName"),
		Entry("missing aws.region", validAWSSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.AWS.Region = ""
		}, "spec.credential.aws.region"),
		Entry("missing aws.roleArn", validAWSSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.AWS.RoleARN = ""
		}, "spec.credential.aws.roleArn"),
		Entry("missing aws.externalId", validAWSSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.AWS.ExternalID = ""
		}, "spec.credential.aws.externalId"),
		Entry("kubeconfig arm set alongside type aws", validAWSSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.Kubeconfig = &v1beta1.KubeconfigCredential{
				SecretRef: v1beta1.SecretKeyRef{Name: "prod-us-east-1-kubeconfig"},
			}
		}, "spec.credential.kubeconfig"),
		Entry("azure arm set alongside type kubeconfig", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.Azure = &v1beta1.AzureCredential{}
		}, "spec.credential.azure"),
		Entry("gcp arm set alongside type aws", validAWSSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.Credential.GCP = &v1beta1.GCPCredential{}
		}, "spec.credential.gcp"),
		Entry("infraProvisioning set", validKubeconfigSpoke, func(sc *v1beta1.SpokeCluster) {
			sc.Spec.InfraProvisioning = &v1beta1.InfraProvisioning{
				BlueprintRef: &v1beta1.BlueprintReference{Name: "infra-a"},
			}
		}, "spec.infraProvisioning"),
	)
})

var _ = Describe("Default", func() {
	It("defaults secretRef.key and leaves schema-defaulted fields to the schema", func() {
		sc := &v1beta1.SpokeCluster{
			Spec: v1beta1.SpokeClusterSpec{
				Credential: v1beta1.CredentialSpec{
					Type: v1beta1.CredentialTypeKubeconfig,
					Kubeconfig: &v1beta1.KubeconfigCredential{
						SecretRef: v1beta1.SecretKeyRef{Name: "prod-us-east-1-kubeconfig"},
					},
				},
			},
		}

		Default(sc)

		// secretRef.key is the only default the CRD schema cannot express.
		gomega.Expect(sc.Spec.Credential.Kubeconfig.SecretRef.Key).To(gomega.Equal("kubeconfig"))

		// mode, probe knobs, and deletionPolicy carry +kubebuilder:default markers,
		// so the webhook leaves them to the apiserver; setting them here would mask
		// an explicit invalid zero value.
		gomega.Expect(sc.Spec.Mode).To(gomega.BeEmpty())
		gomega.Expect(sc.Spec.ProbeIntervalSeconds).To(gomega.BeZero())
		gomega.Expect(sc.Spec.ProbeTimeoutSeconds).To(gomega.BeZero())
		gomega.Expect(sc.Spec.DeletionPolicy).To(gomega.BeEmpty())
	})

	It("does not touch secretRef.namespace", func() {
		sc := &v1beta1.SpokeCluster{
			Spec: v1beta1.SpokeClusterSpec{
				Credential: v1beta1.CredentialSpec{
					Type: v1beta1.CredentialTypeKubeconfig,
					Kubeconfig: &v1beta1.KubeconfigCredential{
						SecretRef: v1beta1.SecretKeyRef{Name: "prod-us-east-1-kubeconfig"},
					},
				},
			},
		}

		Default(sc)

		gomega.Expect(sc.Spec.Credential.Kubeconfig.SecretRef.Namespace).To(gomega.BeEmpty())
	})

	It("does not overwrite explicit values", func() {
		sc := &v1beta1.SpokeCluster{
			Spec: v1beta1.SpokeClusterSpec{
				Mode:                 v1beta1.SpokeClusterModeConnect,
				ProbeIntervalSeconds: 60,
				ProbeTimeoutSeconds:  20,
				DeletionPolicy:       v1beta1.SpokeDeletionPolicyOrphan,
				Credential: v1beta1.CredentialSpec{
					Type: v1beta1.CredentialTypeAWS,
					AWS: &v1beta1.AWSCredential{
						AuthMode:    v1beta1.AWSAuthModeIRSA,
						ClusterName: "prod-us-east-1",
						Region:      "us-east-1",
						RoleARN:     "arn:aws:iam::123456789012:role/per-cluster-role",
						ExternalID:  "us-east-1/123456789012/hub/vela-system/vela-core-cluster-core",
					},
				},
			},
		}

		Default(sc)

		gomega.Expect(sc.Spec.ProbeIntervalSeconds).To(gomega.Equal(int32(60)))
		gomega.Expect(sc.Spec.ProbeTimeoutSeconds).To(gomega.Equal(int32(20)))
		gomega.Expect(sc.Spec.DeletionPolicy).To(gomega.Equal(v1beta1.SpokeDeletionPolicyOrphan))
	})

	It("does not set secretRef.key when kubeconfig arm is unset", func() {
		sc := &v1beta1.SpokeCluster{
			Spec: v1beta1.SpokeClusterSpec{
				Credential: v1beta1.CredentialSpec{
					Type: v1beta1.CredentialTypeAWS,
					AWS: &v1beta1.AWSCredential{
						AuthMode:    v1beta1.AWSAuthModePodIdentity,
						ClusterName: "prod-us-east-1",
						Region:      "us-east-1",
						RoleARN:     "arn:aws:iam::123456789012:role/per-cluster-role",
						ExternalID:  "us-east-1/123456789012/hub/vela-system/vela-core-cluster-core",
					},
				},
			},
		}

		gomega.Expect(func() { Default(sc) }).NotTo(gomega.Panic())
	})
})
