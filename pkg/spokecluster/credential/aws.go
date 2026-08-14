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
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

// eksDescribeAPI is the subset of the EKS API the provider needs: resolve the cluster endpoint
// and CA. It is an interface so tests can stub it without calling AWS.
type eksDescribeAPI interface {
	DescribeCluster(ctx context.Context, params *eks.DescribeClusterInput, optFns ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
}

// awsClientFactory builds the per-cluster AWS clients after the controller assumes the scoped
// role. It is a field on the provider so tests can inject fakes; the default implementation uses
// the ambient Pod Identity / IRSA base identity and chains into the per-cluster role via STS.
type awsClientFactory func(ctx context.Context, cred *v1beta1.AWSCredential) (eksDescribeAPI, stsPresignAPI, error)

// AWSProvider resolves connectivity to an EKS spoke through AWS workload identity. It assumes the
// per-cluster role, describes the cluster for its endpoint and CA, and mints a short-lived EKS
// bearer token that the controller refreshes before expiry.
type AWSProvider struct {
	// newClients builds the EKS and STS-presign clients for a given credential.
	newClients awsClientFactory
	// now returns the current time; overridable in tests for deterministic refresh math.
	now func() time.Time
}

// NewAWSProvider builds an AWS provider that uses the ambient base identity (EKS Pod Identity or
// IRSA on the hub controller pod) and assumes the per-cluster role declared on each SpokeCluster.
func NewAWSProvider() *AWSProvider {
	return &AWSProvider{
		newClients: defaultAWSClientFactory,
		now:        time.Now,
	}
}

// Type returns the aws credential type.
func (p *AWSProvider) Type() v1beta1.CredentialType { return v1beta1.CredentialTypeAWS }

// Materialize assumes the per-cluster role, describes the EKS cluster for its endpoint and CA, and
// mints an EKS bearer token with a refresh deadline. cli is unused for the aws arm (no source
// secret) but kept to satisfy the Provider interface.
func (p *AWSProvider) Materialize(ctx context.Context, _ client.Reader, sc *v1beta1.SpokeCluster) (*Materialized, error) {
	cred := sc.Spec.Credential.AWS
	if cred == nil {
		return nil, fmt.Errorf("credential.aws is required when type is aws")
	}
	if cred.ClusterName == "" || cred.Region == "" || cred.RoleARN == "" {
		return nil, fmt.Errorf("credential.aws requires clusterName, region, and roleArn")
	}

	eksClient, presigner, err := p.newClients(ctx, cred)
	if err != nil {
		return nil, fmt.Errorf("failed to build AWS clients for cluster %q: %w", cred.ClusterName, err)
	}

	out, err := eksClient.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &cred.ClusterName})
	if err != nil {
		return nil, fmt.Errorf("eks:DescribeCluster failed for %q: %w", cred.ClusterName, err)
	}
	if out.Cluster == nil || out.Cluster.Endpoint == nil || out.Cluster.CertificateAuthority == nil || out.Cluster.CertificateAuthority.Data == nil {
		return nil, fmt.Errorf("eks:DescribeCluster returned incomplete data for %q", cred.ClusterName)
	}
	caData, err := base64.StdEncoding.DecodeString(*out.Cluster.CertificateAuthority.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to decode CA data for %q: %w", cred.ClusterName, err)
	}

	endpoint := *out.Cluster.Endpoint
	if err := ValidateSpokeEndpoint(endpoint); err != nil {
		return nil, err
	}

	token, refreshAt, err := generateEKSToken(ctx, presigner, cred.ClusterName, p.now())
	if err != nil {
		return nil, err
	}

	return &Materialized{
		Endpoint:    endpoint,
		CAData:      caData,
		Token:       token,
		Region:      cred.Region,
		NextRefresh: refreshAt,
	}, nil
}

// defaultAWSClientFactory loads the ambient AWS config (Pod Identity / IRSA base identity),
// assumes the per-cluster role (optionally with an external id), and returns EKS and STS-presign
// clients scoped to that role.
func defaultAWSClientFactory(ctx context.Context, cred *v1beta1.AWSCredential) (eksDescribeAPI, stsPresignAPI, error) {
	base, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cred.Region))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load base AWS config: %w", err)
	}
	stsBase := sts.NewFromConfig(base)
	assumed := stscreds.NewAssumeRoleProvider(stsBase, cred.RoleARN, func(o *stscreds.AssumeRoleOptions) {
		if cred.ExternalID != "" {
			o.ExternalID = &cred.ExternalID
		}
	})
	// Derive from base instead of building a fresh Config. base carries BaseEndpoint
	// (from AWS_ENDPOINT_URL), the HTTP client and the retryer; a struct literal drops
	// all of it and silently sends eks:DescribeCluster to the default AWS endpoint.
	scoped := base.Copy()
	scoped.Region = cred.Region
	scoped.Credentials = aws.NewCredentialsCache(assumed)
	return eks.NewFromConfig(scoped), signerFromCredentials(cred.Region, scoped.Credentials), nil
}
