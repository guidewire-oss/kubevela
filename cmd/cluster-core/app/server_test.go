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

package app

import (
	"errors"
	"time"

	clustercommon "github.com/oam-dev/cluster-gateway/pkg/common"
	. "github.com/onsi/ginkgo/v2"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/features"
)

var _ = It("DefaultOptions", func() {
	t := GinkgoT()
	o := defaultOptions()

	assert.Equal(t, ":8080", o.metricsAddr)
	assert.Equal(t, ":9440", o.healthAddr)
	assert.False(t, o.enableLeaderElection)
	assert.Equal(t, "", o.leaderElectionNS)
	assert.Equal(t, 30*time.Second, o.leaseDuration)
	assert.Equal(t, 20*time.Second, o.renewDeadline)
	assert.Equal(t, 5*time.Second, o.retryPeriod)
	assert.False(t, o.useWebhook)
	assert.Equal(t, 9445, o.webhookPort)
	assert.Equal(t, "/k8s-webhook-server/serving-certs", o.certDir)
	assert.Equal(t, 5, o.concurrentReconciles)
	assert.False(t, o.autoUpgradeSecret)
	assert.Equal(t, 15*time.Minute, o.credentialCacheTTL)
	assert.Equal(t, "", o.gatewaySecretNS)
})

var _ = It("AddFlags", func() {
	t := GinkgoT()
	o := defaultOptions()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	addFlags(fs, o)

	for _, name := range []string{
		"metrics-bind-address",
		"health-probe-bind-address",
		"enable-leader-election",
		"leader-election-namespace",
		"leader-election-lease-duration",
		"leader-election-renew-deadline",
		"leader-election-retry-period",
		"use-webhook",
		"webhook-port",
		"webhook-cert-dir",
		"concurrent-reconciles",
		"auto-upgrade-cluster-secret",
		"credential-cache-ttl",
		"cluster-gateway-secret-namespace",
		"feature-gates",
	} {
		f := fs.Lookup(name)
		require.NotNilf(t, f, "expected flag %q to be registered", name)
	}
})

var _ = It("AddFlagsBindsLeaderElectionDurations", func() {
	t := GinkgoT()
	o := defaultOptions()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	addFlags(fs, o)

	require.NoError(t, fs.Parse([]string{
		"--leader-election-lease-duration=45s",
		"--leader-election-renew-deadline=30s",
		"--leader-election-retry-period=7s",
	}))

	assert.Equal(t, 45*time.Second, o.leaseDuration)
	assert.Equal(t, 30*time.Second, o.renewDeadline)
	assert.Equal(t, 7*time.Second, o.retryPeriod)
})

var _ = It("ConfigureSpokeClusterWebhooksRequiresBothGates", func() {
	t := GinkgoT()
	tests := []struct {
		name               string
		featureGateEnabled bool
		useWebhook         bool
		expectedCalls      []string
	}{
		{
			name:               "feature gate off",
			featureGateEnabled: false,
			useWebhook:         true,
		},
		{
			name:               "use-webhook off",
			featureGateEnabled: true,
			useWebhook:         false,
		},
		{
			name:               "both gates on",
			featureGateEnabled: true,
			useWebhook:         true,
			expectedCalls:      []string{"wait", "validating", "mutating"},
		},
	}

	for _, tt := range tests {
		By(tt.name, func() {
			featuregatetesting.SetFeatureGateDuringTest(
				t,
				utilfeature.DefaultFeatureGate,
				features.EnableClusterInfrastructure,
				tt.featureGateEnabled,
			)
			o := defaultOptions()
			o.useWebhook = tt.useWebhook

			var calls []string
			err := configureSpokeClusterWebhooks(
				o,
				func() error {
					calls = append(calls, "wait")
					return nil
				},
				func() {
					calls = append(calls, "validating")
				},
				func() {
					calls = append(calls, "mutating")
				},
			)

			require.NoError(t, err)
			assert.Equal(t, tt.expectedCalls, calls)
		})
	}
})

var _ = It("ConfigureSpokeClusterWebhooksStopsWhenCertWaitFails", func() {
	t := GinkgoT()
	featuregatetesting.SetFeatureGateDuringTest(
		t,
		utilfeature.DefaultFeatureGate,
		features.EnableClusterInfrastructure,
		true,
	)
	o := defaultOptions()
	o.useWebhook = true
	waitErr := errors.New("certificate unavailable")

	var calls []string
	err := configureSpokeClusterWebhooks(
		o,
		func() error {
			calls = append(calls, "wait")
			return waitErr
		},
		func() {
			calls = append(calls, "validating")
		},
		func() {
			calls = append(calls, "mutating")
		},
	)

	assert.ErrorIs(t, err, waitErr)
	assert.Equal(t, []string{"wait"}, calls)
})

var _ = It("ResolveGatewaySecretNamespacePrefersConfigured", func() {
	t := GinkgoT()
	assert.Equal(t, "kubevela", resolveGatewaySecretNamespace("vela-system", "kubevela"))
	assert.Equal(t, "vela-system", resolveGatewaySecretNamespace("vela-system", ""))
	assert.Equal(t, types.DefaultKubeVelaNS, resolveGatewaySecretNamespace("", ""))
})

var _ = It("StripNonCredentialSecretDataKeepsCredentialPayloads", func() {
	t := GinkgoT()
	cred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "spoke",
			Labels: map[string]string{
				clustercommon.LabelKeyClusterCredentialType: "ServiceAccountToken",
			},
		},
		Data: map[string][]byte{"token": []byte("tok"), "ca.crt": []byte("ca")},
	}
	got, err := stripNonCredentialSecretData(cred)
	require.NoError(t, err)
	out, ok := got.(*corev1.Secret)
	require.True(t, ok)
	assert.Equal(t, []byte("tok"), out.Data["token"])

	tls := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vela-core-cluster-core-admission"},
		Data:       map[string][]byte{"tls.key": []byte("private")},
	}
	got, err = stripNonCredentialSecretData(tls)
	require.NoError(t, err)
	out, ok = got.(*corev1.Secret)
	require.True(t, ok)
	assert.Equal(t, "vela-core-cluster-core-admission", out.Name)
	assert.Nil(t, out.Data)
})
