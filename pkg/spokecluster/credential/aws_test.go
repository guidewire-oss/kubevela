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

package credential

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	. "github.com/onsi/ginkgo/v2"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

// fakeEKS returns a canned DescribeCluster response.
type fakeEKS struct {
	out *eks.DescribeClusterOutput
	err error
}

func (f *fakeEKS) DescribeCluster(_ context.Context, _ *eks.DescribeClusterInput, _ ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

func awsSpoke() *v1beta1.SpokeCluster {
	return &v1beta1.SpokeCluster{
		Spec: v1beta1.SpokeClusterSpec{
			Credential: v1beta1.CredentialSpec{
				Type: v1beta1.CredentialTypeAWS,
				AWS: &v1beta1.AWSCredential{
					AuthMode:    v1beta1.AWSAuthModePodIdentity,
					ClusterName: "prod-us-east-1",
					Region:      "us-east-1",
					RoleARN:     "arn:aws:iam::123456789012:role/spoke-scoped",
				},
			},
		},
	}
}

func strptr(s string) *string { return &s }

// noAmbientAWSHints keeps Materialize specs independent of the runner's IRSA /
// Pod Identity environment.
func noAmbientAWSHints() (bool, bool) { return false, false }

var _ = It("AWSProviderMaterialize", func() {
	t := GinkgoT()
	now := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	caB64 := base64.StdEncoding.EncodeToString([]byte("spoke-ca-pem"))
	presignedURL := "https://sts.amazonaws.com/?Action=GetCallerIdentity"

	p := &AWSProvider{
		now:          func() time.Time { return now },
		ambientHints: noAmbientAWSHints,
		newClients: func(_ context.Context, _ *v1beta1.AWSCredential) (eksDescribeAPI, stsPresignAPI, error) {
			ek := &fakeEKS{out: &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{
				Endpoint:             strptr("https://XYZ.eks.amazonaws.com"),
				CertificateAuthority: &ekstypes.Certificate{Data: strptr(caB64)},
			}}}
			return ek, &fakePresigner{url: presignedURL}, nil
		},
	}

	m, err := p.Materialize(context.Background(), nil, awsSpoke())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.Endpoint != "https://XYZ.eks.amazonaws.com" {
		t.Fatalf("endpoint = %q", m.Endpoint)
	}
	if string(m.CAData) != "spoke-ca-pem" {
		t.Fatalf("ca = %q", string(m.CAData))
	}
	if m.Token == "" {
		t.Fatal("expected a minted token")
	}
	if m.Region != "us-east-1" {
		t.Fatalf("region = %q", m.Region)
	}
	// The EKS token must schedule a refresh 13 minutes out.
	if want := now.Add(13 * time.Minute); !m.NextRefresh.Equal(want) {
		t.Fatalf("nextRefresh = %v, want %v", m.NextRefresh, want)
	}
})

var _ = It("AWSProviderValidation", func() {
	t := GinkgoT()
	p := NewAWSProvider()
	p.ambientHints = noAmbientAWSHints

	// Missing aws arm.
	scNoArm := &v1beta1.SpokeCluster{Spec: v1beta1.SpokeClusterSpec{Credential: v1beta1.CredentialSpec{Type: v1beta1.CredentialTypeAWS}}}
	if _, err := p.Materialize(context.Background(), nil, scNoArm); err == nil {
		t.Fatal("expected error when aws arm is nil")
	}

	// Missing required fields.
	scPartial := awsSpoke()
	scPartial.Spec.Credential.AWS.RoleARN = ""
	if _, err := p.Materialize(context.Background(), nil, scPartial); err == nil {
		t.Fatal("expected error when roleArn is empty")
	}
})

var _ = It("AWSProviderDescribeFailure", func() {
	t := GinkgoT()
	p := &AWSProvider{
		now:          time.Now,
		ambientHints: noAmbientAWSHints,
		newClients: func(_ context.Context, _ *v1beta1.AWSCredential) (eksDescribeAPI, stsPresignAPI, error) {
			return &fakeEKS{err: errors.New("access denied")}, &fakePresigner{url: "https://x"}, nil
		},
	}
	if _, err := p.Materialize(context.Background(), nil, awsSpoke()); err == nil {
		t.Fatal("expected error when DescribeCluster fails")
	}
})

var _ = It("AWSProviderRejectsBlockedEndpoint", func() {
	t := GinkgoT()
	caB64 := base64.StdEncoding.EncodeToString([]byte("spoke-ca-pem"))
	p := &AWSProvider{
		now:          time.Now,
		ambientHints: noAmbientAWSHints,
		newClients: func(_ context.Context, _ *v1beta1.AWSCredential) (eksDescribeAPI, stsPresignAPI, error) {
			// Defense in depth: even if DescribeCluster returned a metadata IP, refuse it.
			ek := &fakeEKS{out: &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{
				Endpoint:             strptr("https://169.254.169.254/"),
				CertificateAuthority: &ekstypes.Certificate{Data: strptr(caB64)},
			}}}
			return ek, &fakePresigner{url: "https://sts.amazonaws.com/?Action=GetCallerIdentity"}, nil
		},
	}
	_, err := p.Materialize(context.Background(), nil, awsSpoke())
	if err == nil {
		t.Fatal("expected endpoint policy error")
	}
	if !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("error = %q, want not permitted", err.Error())
	}
})

var _ = It("AWSProviderIncompleteCluster", func() {
	t := GinkgoT()
	p := &AWSProvider{
		now:          time.Now,
		ambientHints: noAmbientAWSHints,
		newClients: func(_ context.Context, _ *v1beta1.AWSCredential) (eksDescribeAPI, stsPresignAPI, error) {
			// Endpoint present but CA missing.
			ek := &fakeEKS{out: &eks.DescribeClusterOutput{Cluster: &ekstypes.Cluster{Endpoint: strptr("https://x")}}}
			return ek, &fakePresigner{url: "https://x"}, nil
		},
	}
	if _, err := p.Materialize(context.Background(), nil, awsSpoke()); err == nil {
		t.Fatal("expected error when cluster data is incomplete")
	}
})

var _ = It("AssertAWSAuthModeMatchesAmbient", func() {
	t := GinkgoT()
	p := &AWSProvider{ambientHints: ambientAWSIdentityHints}

	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	t.Setenv("AWS_ROLE_ARN", "")
	if err := p.assertAuthModeMatchesAmbient(v1beta1.AWSAuthModePodIdentity); err != nil {
		t.Fatalf("no ambient hints should accept either mode: %v", err)
	}
	if err := p.assertAuthModeMatchesAmbient(v1beta1.AWSAuthModeIRSA); err != nil {
		t.Fatalf("no ambient hints should accept either mode: %v", err)
	}

	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "/var/run/secrets/eks.amazonaws.com/serviceaccount/token")
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::1:role/hub")
	if err := p.assertAuthModeMatchesAmbient(v1beta1.AWSAuthModePodIdentity); err == nil {
		t.Fatal("podIdentity must fail when ambient identity looks like IRSA")
	}
	if err := p.assertAuthModeMatchesAmbient(v1beta1.AWSAuthModeIRSA); err != nil {
		t.Fatalf("irsa should accept IRSA ambient hints: %v", err)
	}

	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	t.Setenv("AWS_ROLE_ARN", "")
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", "http://169.254.170.23/v1/credentials")
	if err := p.assertAuthModeMatchesAmbient(v1beta1.AWSAuthModeIRSA); err == nil {
		t.Fatal("irsa must fail when ambient identity looks like Pod Identity")
	}
	if err := p.assertAuthModeMatchesAmbient(v1beta1.AWSAuthModePodIdentity); err != nil {
		t.Fatalf("podIdentity should accept Pod Identity ambient hints: %v", err)
	}
})
