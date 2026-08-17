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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

// errMaterialize stands in for any provider failure; only its non-nilness matters.
var errMaterialize = errors.New("materialize failed")

// resetSpokeMetrics clears every family so a spec never inherits another spec's counters.
// The collectors are package-level, so no spec may depend on ordering.
func resetSpokeMetrics() {
	spokeConnectionTransitions.Reset()
	spokeConditionFailures.Reset()
	spokeDetachTotal.Reset()
	spokeConnected.Reset()
	spokeNodeCount.Reset()
	spokeProbeLatency.Reset()
	spokeCredentialRefresh.Reset()
}

func metricSpoke(namespace, name string) *v1beta1.SpokeCluster {
	return &v1beta1.SpokeCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// histogramValues reads one spoke's probe-latency histogram back out of the registered
// collector. testutil.ToFloat64 handles only single-value metrics, so a histogram needs
// the dto round-trip.
func histogramValues(sc *v1beta1.SpokeCluster) (uint64, float64) {
	observer, ok := spokeProbeLatency.With(spokeMetricLabels(sc)).(prometheus.Metric)
	Expect(ok).To(BeTrue(), "probe latency observer must also be a prometheus.Metric")
	var out dto.Metric
	Expect(observer.Write(&out)).To(Succeed())
	return out.GetHistogram().GetSampleCount(), out.GetHistogram().GetSampleSum()
}

// Requirement 1: the three families that shipped as fleet aggregates now name the spoke
// they came from, and one spoke's series cannot be read as another's.
var _ = It("TransitionCountersAreLabelledByCluster", func() {
	resetSpokeMetrics()
	rec := &recordingRecorder{}
	r := &Reconciler{record: rec}
	sc := metricSpoke("tenant-a", "spoke-1")

	prev := &v1beta1.SpokeClusterStatus{Connection: v1beta1.ConnectionStateUnknown}
	next := &v1beta1.SpokeClusterStatus{Connection: v1beta1.ConnectionStateConnected}
	setCondition(next, v1beta1.SpokeClusterConditionConnected, metav1.ConditionTrue, reasonProbeSucceeded, "up")
	r.emitStatusEvents(sc, prev, next)

	Expect(testutil.ToFloat64(spokeConnectionTransitions.WithLabelValues(
		sc.Namespace, sc.Name, string(v1beta1.ConnectionStateConnected)))).To(Equal(1.0))

	By("keeping a same-named spoke in another namespace on its own series", func() {
		other := metricSpoke("tenant-b", "spoke-1")
		Expect(testutil.ToFloat64(spokeConnectionTransitions.WithLabelValues(
			other.Namespace, other.Name, string(v1beta1.ConnectionStateConnected)))).To(BeZero())
	})

	By("labelling condition failures too", func() {
		failed := &v1beta1.SpokeClusterStatus{}
		setCondition(failed, v1beta1.SpokeClusterConditionCredentialValid, metav1.ConditionFalse,
			reasonMaterializeFailed, "bad kubeconfig")
		r.emitStatusEvents(sc, &v1beta1.SpokeClusterStatus{}, failed)
		Expect(testutil.ToFloat64(spokeConditionFailures.WithLabelValues(
			sc.Namespace, sc.Name, v1beta1.SpokeClusterConditionCredentialValid, reasonMaterializeFailed))).To(Equal(1.0))
	})
})

// Requirement 2: only Connected reads 1. Unknown reads 0 because it does not assert an
// observed-reachable spoke, which is exactly the distinction status.connection draws.
var _ = It("ObserveConnectionGauge", func() {
	sc := metricSpoke("vela-system", "spoke-gauge")
	tests := []struct {
		name  string
		state v1beta1.ConnectionState
		want  float64
	}{
		{name: "connected", state: v1beta1.ConnectionStateConnected, want: 1},
		{name: "disconnected", state: v1beta1.ConnectionStateDisconnected, want: 0},
		{name: "unknown", state: v1beta1.ConnectionStateUnknown, want: 0},
		{name: "unset", state: "", want: 0},
	}
	for _, tt := range tests {
		By(tt.name, func() {
			resetSpokeMetrics()
			observeConnection(sc, &v1beta1.SpokeClusterStatus{Connection: tt.state})
			Expect(testutil.ToFloat64(spokeConnected.With(spokeMetricLabels(sc)))).To(Equal(tt.want),
				"spokecluster_connected for state %q", tt.state)
		})
	}
})

// Requirements 3 and 4.
var _ = It("ObserveInventoryAndLatency", func() {
	resetSpokeMetrics()
	sc := metricSpoke("vela-system", "spoke-inventory")

	observeInventory(sc, &v1beta1.SpokeClusterInfo{NodeCount: 3})
	Expect(testutil.ToFloat64(spokeNodeCount.With(spokeMetricLabels(sc)))).To(Equal(3.0))

	By("keeping the last known node count when discovery fails", func() {
		// Requirement 3.1: matches status, where the previous clusterInfo is kept and
		// marked stale rather than blanked.
		observeInventory(sc, nil)
		Expect(testutil.ToFloat64(spokeNodeCount.With(spokeMetricLabels(sc)))).To(Equal(3.0))
	})

	By("observing probe latency in seconds", func() {
		observeProbeLatency(sc, 250*time.Millisecond)
		Expect(testutil.CollectAndCount(spokeProbeLatency)).To(Equal(1))
		Expect(testutil.CollectAndLint(spokeProbeLatency)).To(BeEmpty())
		count, sum := histogramValues(sc)
		Expect(count).To(Equal(uint64(1)))
		// Requirement 4.1: a 250ms probe sums to 0.25, not to 250.
		Expect(sum).To(Equal(0.25))
	})
})

// Requirement 5, including the negative case that gives the counter its meaning: a
// credential cache hit did no provider work and must not be counted as a refresh.
var _ = It("ObserveCredentialRefresh", func() {
	resetSpokeMetrics()
	sc := metricSpoke("tenant-a", "spoke-aws")
	sc.Spec.Credential.Type = v1beta1.CredentialTypeAWS
	awsType := string(v1beta1.CredentialTypeAWS)

	observeCredentialRefresh(sc, nil)
	Expect(testutil.ToFloat64(spokeCredentialRefresh.WithLabelValues(
		sc.Namespace, sc.Name, awsType, metricResultSuccess))).To(Equal(1.0))

	observeCredentialRefresh(sc, errMaterialize)
	Expect(testutil.ToFloat64(spokeCredentialRefresh.WithLabelValues(
		sc.Namespace, sc.Name, awsType, metricResultError))).To(Equal(1.0))
	Expect(testutil.ToFloat64(spokeCredentialRefresh.WithLabelValues(
		sc.Namespace, sc.Name, awsType, metricResultSuccess))).To(Equal(1.0),
		"a failure must not touch the success series")
})

// Requirement 7: a deleted spoke leaves no series, so cardinality tracks live spokes
// rather than every spoke that ever existed.
var _ = It("ForgetSpokeMetrics", func() {
	resetSpokeMetrics()
	sc := metricSpoke("tenant-a", "spoke-gone")
	sc.Spec.Credential.Type = v1beta1.CredentialTypeKubeconfig
	survivor := metricSpoke("tenant-a", "spoke-stays")

	for _, spoke := range []*v1beta1.SpokeCluster{sc, survivor} {
		observeConnection(spoke, &v1beta1.SpokeClusterStatus{Connection: v1beta1.ConnectionStateConnected})
		observeInventory(spoke, &v1beta1.SpokeClusterInfo{NodeCount: 1})
		observeProbeLatency(spoke, time.Second)
		observeCredentialRefresh(spoke, nil)
		spokeConnectionTransitions.WithLabelValues(spoke.Namespace, spoke.Name,
			string(v1beta1.ConnectionStateConnected)).Inc()
		spokeConditionFailures.WithLabelValues(spoke.Namespace, spoke.Name,
			v1beta1.SpokeClusterConditionConnected, reasonProbeFailed).Inc()
		spokeDetachTotal.WithLabelValues(spoke.Namespace, spoke.Name, metricResultSuccess).Inc()
	}

	forgetSpokeMetrics(sc)

	for _, collector := range []prometheus.Collector{
		spokeConnected, spokeNodeCount, spokeProbeLatency,
		spokeConnectionTransitions, spokeConditionFailures, spokeDetachTotal, spokeCredentialRefresh,
	} {
		Expect(testutil.CollectAndCount(collector)).To(Equal(1),
			"only the surviving spoke's series may remain")
	}
	Expect(testutil.ToFloat64(spokeConnected.With(spokeMetricLabels(survivor)))).To(Equal(1.0))
})
