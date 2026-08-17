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

package v1beta1

import (
	"context"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// splitYAMLDocuments splits a multi-document YAML file (separated by "---")
// into its individual document strings.
func splitYAMLDocuments(t GinkgoTInterface, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var docs []string
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		if strings.TrimSpace(doc) != "" {
			docs = append(docs, doc)
		}
	}
	return docs
}

// Both envtest specs share one control plane. Starting a kube-apiserver and etcd
// takes the better part of a minute, and nothing either spec does needs isolation
// from the other: they use distinct object names inside one namespace.
//
// ContinueOnFailure because Ordered otherwise skips the remaining specs once one
// fails, and these two are independent. Without it a changed example document
// failing InstallAndApply would silently take the CEL rejection coverage with it.
var _ = Describe("SpokeClusterCRD envtest", Ordered, ContinueOnFailure, func() {
	var (
		testEnv   *envtest.Environment
		cfg       *rest.Config
		k8sClient client.Client
	)

	BeforeAll(func() {
		r := require.New(GinkgoT())
		if os.Getenv("KUBEBUILDER_ASSETS") == "" {
			Skip("KUBEBUILDER_ASSETS not set; skipping envtest specs")
		}

		testEnv = &envtest.Environment{
			ControlPlaneStartTimeout: 2 * time.Minute,
			ControlPlaneStopTimeout:  time.Minute,
			UseExistingCluster:       ptr.To(false),
			CRDDirectoryPaths:        []string{spokeClusterCRDPath},
		}

		var err error
		cfg, err = testEnv.Start()
		r.NoError(err, "envtest environment must start (requires KUBEBUILDER_ASSETS)")

		k8sClient, err = client.New(cfg, client.Options{Scheme: k8sscheme.Scheme})
		r.NoError(err)
		r.NoError(k8sClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "vela-system"},
		}))
	})

	AfterAll(func() {
		if testEnv != nil {
			require.NoError(GinkgoT(), testEnv.Stop())
		}
	})

	// Installs the generated SpokeCluster CRD, applies both worked examples, and
	// retrieves them back (Requirement 1, criterion 1; Requirement 7, criterion 1).
	It("InstallAndApply", func() {
		t := GinkgoT()
		r := require.New(t)
		ctx := context.Background()

		// The installed CRD must be namespaced: kubectl get spokeclusters lists
		// within a namespace, not cluster-wide (Requirement 1, criterion 3).
		disco, err := discovery.NewDiscoveryClientForConfig(cfg)
		r.NoError(err)
		resources, err := disco.ServerResourcesForGroupVersion(SchemeGroupVersion.String())
		r.NoError(err)
		var found bool
		for _, res := range resources.APIResources {
			if res.Name == "spokeclusters" {
				found = true
				r.True(res.Namespaced, "spokeclusters must be namespaced")
			}
		}
		r.True(found, "spokeclusters resource must be registered with the API server")

		// AWS Pod Identity example.
		awsDocs := splitYAMLDocuments(t, "../../../docs/examples/spokecluster-connect/spokecluster-aws.yaml")
		r.Len(awsDocs, 1)
		awsCluster := &SpokeCluster{}
		r.NoError(yaml.Unmarshal([]byte(awsDocs[0]), awsCluster))
		r.NoError(k8sClient.Create(ctx, awsCluster))

		gotAWS := &SpokeCluster{}
		r.NoError(k8sClient.Get(ctx, types.NamespacedName{Name: "prod-us-east-1", Namespace: "vela-system"}, gotAWS))
		r.Equal(SpokeClusterModeConnect, gotAWS.Spec.Mode)
		r.Equal(CredentialTypeAWS, gotAWS.Spec.Credential.Type)

		// Static kubeconfig example: a source Secret plus the SpokeCluster that
		// references it.
		kubeconfigDocs := splitYAMLDocuments(t, "../../../docs/examples/spokecluster-connect/spokecluster-kubeconfig.yaml")
		r.Len(kubeconfigDocs, 2)

		secret := &corev1.Secret{}
		r.NoError(yaml.Unmarshal([]byte(kubeconfigDocs[0]), secret))
		r.NoError(k8sClient.Create(ctx, secret))

		kubeconfigCluster := &SpokeCluster{}
		r.NoError(yaml.Unmarshal([]byte(kubeconfigDocs[1]), kubeconfigCluster))
		r.NoError(k8sClient.Create(ctx, kubeconfigCluster))

		gotKubeconfig := &SpokeCluster{}
		r.NoError(k8sClient.Get(ctx, types.NamespacedName{Name: "dev-spoke", Namespace: "vela-system"}, gotKubeconfig))
		r.Equal(CredentialTypeKubeconfig, gotKubeconfig.Spec.Credential.Type)
		r.Equal("dev-spoke-kubeconfig", gotKubeconfig.Spec.Credential.Kubeconfig.SecretRef.Name)

		// kubectl get spokeclusters must list both, cluster-wide retrieval within
		// the namespace (Requirement 1, criterion 1).
		list := &SpokeClusterList{}
		r.NoError(k8sClient.List(ctx, list, client.InNamespace("vela-system")))
		r.Len(list.Items, 2)
	})

	// The CEL rules have to be exercised against a real API server, because that is
	// the thing evaluating them. No webhook runs here, which is the point: the chart
	// ships the webhook off by default, so these rejections are what a default
	// install actually enforces at apply time.
	It("CELRejectsInvalidSpecs", func() {
		t := GinkgoT()
		r := require.New(t)
		ctx := context.Background()

		kubeconfigArm := func() *KubeconfigCredential {
			return &KubeconfigCredential{SecretRef: SecretKeyRef{Name: "some-kubeconfig"}}
		}
		awsArm := func() *AWSCredential {
			return &AWSCredential{
				AuthMode:    AWSAuthModePodIdentity,
				ClusterName: "spoke",
				Region:      "us-east-1",
				RoleARN:     "arn:aws:iam::111122223333:role/spoke",
				ExternalID:  "external",
			}
		}

		cases := []struct {
			name string
			want string
			spec SpokeClusterSpec
		}{
			{
				name: "arm-does-not-match-type",
				want: "when type is 'kubeconfig'",
				spec: SpokeClusterSpec{
					Mode:       SpokeClusterModeConnect,
					Credential: CredentialSpec{Type: CredentialTypeKubeconfig, AWS: awsArm()},
				},
			},
			{
				name: "no-arm-at-all",
				want: "when type is 'aws'",
				spec: SpokeClusterSpec{
					Mode:       SpokeClusterModeConnect,
					Credential: CredentialSpec{Type: CredentialTypeAWS},
				},
			},
			{
				name: "two-arms-set",
				want: "no other arm may be set",
				spec: SpokeClusterSpec{
					Mode: SpokeClusterModeConnect,
					Credential: CredentialSpec{
						Type: CredentialTypeKubeconfig, Kubeconfig: kubeconfigArm(), AWS: awsArm(),
					},
				},
			},
			{
				name: "phase-2-provisioning-in-connect-mode",
				want: "infraProvisioning is not supported in connect mode",
				spec: SpokeClusterSpec{
					Mode:              SpokeClusterModeConnect,
					Credential:        CredentialSpec{Type: CredentialTypeKubeconfig, Kubeconfig: kubeconfigArm()},
					InfraProvisioning: &InfraProvisioning{BlueprintRef: &BlueprintReference{Name: "infra"}},
				},
			},
		}

		for _, tc := range cases {
			By(tc.name, func() {
				err := k8sClient.Create(ctx, &SpokeCluster{
					ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: "vela-system"},
					Spec:       tc.spec,
				})
				r.Error(err, "the API server must refuse %s with no webhook running", tc.name)
				r.Contains(err.Error(), tc.want, "the rejection has to name what is wrong")
			})
		}

		By("still admitting the Phase 2 stubs that are deliberately accepted", func() {
			// blueprintRef and rolloutStrategyRef are inert in Phase 1 but accepted, so
			// GitOps can land the Phase 2 shape early. Only infraProvisioning is refused.
			r.NoError(k8sClient.Create(ctx, &SpokeCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "accepted-stubs", Namespace: "vela-system"},
				Spec: SpokeClusterSpec{
					Mode:               SpokeClusterModeConnect,
					Credential:         CredentialSpec{Type: CredentialTypeKubeconfig, Kubeconfig: kubeconfigArm()},
					BlueprintRef:       &BlueprintReference{Name: "blueprint"},
					RolloutStrategyRef: &BlueprintReference{Name: "rollout"},
				},
			}))
		})
	})
})
