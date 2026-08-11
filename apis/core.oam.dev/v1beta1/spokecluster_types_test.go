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
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// sampleAWSSpokeCluster returns a fully-populated AWS Pod Identity SpokeCluster.
func sampleAWSSpokeCluster() *SpokeCluster {
	return &SpokeCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: SchemeGroupVersion.String(),
			Kind:       "SpokeCluster",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "prod-us-east-1", Namespace: "vela-system"},
		Spec: SpokeClusterSpec{
			Mode: SpokeClusterModeConnect,
			Credential: CredentialSpec{
				Type: CredentialTypeAWS,
				AWS: &AWSCredential{
					AuthMode:    AWSAuthModePodIdentity,
					ClusterName: "prod-us-east-1",
					Region:      "us-east-1",
					RoleARN:     "arn:aws:iam::123456789012:role/per-cluster-role",
				},
			},
		},
		Status: SpokeClusterStatus{
			Connection: ConnectionStateConnected,
			ClusterInfo: &SpokeClusterInfo{
				KubernetesVersion: "v1.30.0",
				Platform:          "eks",
				Region:            "us-east-1",
				NodeCount:         3,
			},
		},
	}
}

// TestSpokeCluster_JSONRoundTrip proves the type and its credential union
// round-trip through JSON without losing fields.
var _ = It("SpokeCluster JSONRoundTrip", func() {
	t := GinkgoT()
	r := require.New(t)
	original := sampleAWSSpokeCluster()

	data, err := json.Marshal(original)
	r.NoError(err)

	decoded := &SpokeCluster{}
	r.NoError(json.Unmarshal(data, decoded))
	r.Equal(original, decoded)
})

// TestSpokeCluster_DeepCopyRoundTrip proves generated deepcopy methods exist
// and produce an equal, independent copy (Requirement 7, criterion 2).
var _ = It("SpokeCluster DeepCopyRoundTrip", func() {
	t := GinkgoT()
	r := require.New(t)
	original := sampleAWSSpokeCluster()

	cp := original.DeepCopy()
	r.Equal(original, cp)

	// Mutating the copy must not affect the original (independent memory).
	cp.Spec.Credential.AWS.Region = "eu-west-1"
	r.Equal("us-east-1", original.Spec.Credential.AWS.Region)
})

// TestSpokeClusterList_DeepCopy proves the list type carries generated deepcopy.
var _ = It("SpokeClusterList DeepCopy", func() {
	t := GinkgoT()
	r := require.New(t)
	list := &SpokeClusterList{Items: []SpokeCluster{*sampleAWSSpokeCluster()}}
	r.Equal(list, list.DeepCopy())
})

// TestSpokeCluster_RegisteredInScheme proves SpokeCluster and SpokeClusterList
// are registered with the API scheme so the type round-trips (Requirement 7).
var _ = It("SpokeCluster RegisteredInScheme", func() {
	t := GinkgoT()
	r := require.New(t)
	s := runtime.NewScheme()
	r.NoError(AddToScheme(s))

	r.True(s.Recognizes(SchemeGroupVersion.WithKind("SpokeCluster")))
	r.True(s.Recognizes(SchemeGroupVersion.WithKind("SpokeClusterList")))
})

// TestSpokeCluster_KubeconfigCredential proves the kubeconfig arm round-trips
// with an optional Secret namespace (webhook rejects a namespace that does
// not match the SpokeCluster; omit namespace to use the SpokeCluster's).
var _ = It("SpokeCluster KubeconfigCredential", func() {
	t := GinkgoT()
	r := require.New(t)
	sc := &SpokeCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "dev-spoke", Namespace: "vela-system"},
		Spec: SpokeClusterSpec{
			Mode: SpokeClusterModeConnect,
			Credential: CredentialSpec{
				Type: CredentialTypeKubeconfig,
				Kubeconfig: &KubeconfigCredential{
					SecretRef: SecretKeyRef{
						Name: "dev-spoke-kubeconfig",
					},
				},
			},
		},
	}

	data, err := json.Marshal(sc)
	r.NoError(err)

	decoded := &SpokeCluster{}
	r.NoError(json.Unmarshal(data, decoded))
	r.Empty(decoded.Spec.Credential.Kubeconfig.SecretRef.Namespace)

	// SecretRef.Namespace is optional (omitempty): it must not serialize when unset.
	secretRefData, err := json.Marshal(sc.Spec.Credential.Kubeconfig.SecretRef)
	r.NoError(err)
	r.NotContains(string(secretRefData), `"namespace"`)
})
