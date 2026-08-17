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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

// Every family is labelled by the SpokeCluster it describes. A fleet-wide gauge that
// cannot name the unhealthy spoke does not answer the question an operator asks, and both
// label values come from the reconciled object, so a SpokeCluster in a tenant namespace
// reports its own namespace rather than the gateway's.
//
// The cost is deliberate: series count scales with the fleet (times reasons for the
// condition-failure family), so forgetSpokeMetrics drops a spoke's series on deletion to
// keep cardinality tracking live spokes. It also means no family can pre-create its
// children, so nothing appears on /metrics until the first spoke reconciles.
const (
	metricLabelNamespace = "namespace"
	metricLabelName      = "name"
)

// Result label values, shared by the refresh and detach counters so a caller cannot
// invent a third spelling.
const (
	metricResultSuccess = "success"
	metricResultError   = "error"
)

var (
	spokeConnectionTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vela_cluster_connection_transitions_total",
		Help: "SpokeCluster connection state transitions observed by the controller.",
	}, []string{metricLabelNamespace, metricLabelName, "to"})

	spokeConditionFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vela_cluster_condition_failures_total",
		Help: "SpokeCluster condition transitions to False (credential, register or discovery failures).",
	}, []string{metricLabelNamespace, metricLabelName, "condition", "reason"})

	spokeDetachTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vela_cluster_detach_total",
		Help: "SpokeCluster detach (delete) outcomes.",
	}, []string{metricLabelNamespace, metricLabelName, "result"})

	spokeConnected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vela_cluster_connected",
		Help: "1 when the hub last observed the spoke as reachable, 0 otherwise.",
	}, []string{metricLabelNamespace, metricLabelName})

	spokeNodeCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vela_cluster_node_count",
		Help: "Nodes discovered on the spoke at the last successful discovery.",
	}, []string{metricLabelNamespace, metricLabelName})

	// Buckets span a same-network hop through to the 10s default probe timeout, so a
	// spoke answering just under its timeout is distinguishable from a healthy one.
	spokeProbeLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vela_cluster_probe_latency_seconds",
		Help:    "Round-trip duration of a successful spoke reachability probe.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{metricLabelNamespace, metricLabelName})

	spokeCredentialRefresh = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vela_cluster_credential_refresh_total",
		Help: "Credential materializations by the controller, excluding credential cache hits.",
	}, []string{metricLabelNamespace, metricLabelName, "type", "result"})
)

func init() {
	metrics.Registry.MustRegister(
		spokeConnectionTransitions,
		spokeConditionFailures,
		spokeDetachTotal,
		spokeConnected,
		spokeNodeCount,
		spokeProbeLatency,
		spokeCredentialRefresh,
	)
}

// spokeMetricLabels identifies one SpokeCluster's series.
func spokeMetricLabels(sc *v1beta1.SpokeCluster) prometheus.Labels {
	return prometheus.Labels{metricLabelNamespace: sc.Namespace, metricLabelName: sc.Name}
}

// observeConnection publishes the connection state this pass computed.
//
// Called for every pass rather than only for passes that write status: a healthy spoke's
// status write is suppressed as a no-op, and a scrape must still see the current value.
// Anything other than Connected reads 0, because Disconnected, Unknown and Pending all
// mean "not observed reachable", which is the distinction status.connection draws.
func observeConnection(sc *v1beta1.SpokeCluster, status *v1beta1.SpokeClusterStatus) {
	if status == nil {
		return
	}
	value := 0.0
	if status.Connection == v1beta1.ConnectionStateConnected {
		value = 1
	}
	spokeConnected.With(spokeMetricLabels(sc)).Set(value)
}

// observeInventory publishes the discovered node count.
//
// A nil info means discovery failed, and the previous value is left in place rather than
// zeroed: that mirrors status, where the last known clusterInfo is kept and marked stale
// through InfoSynced. Zeroing would read as "the spoke lost its nodes".
func observeInventory(sc *v1beta1.SpokeCluster, info *v1beta1.SpokeClusterInfo) {
	if info == nil {
		return
	}
	spokeNodeCount.With(spokeMetricLabels(sc)).Set(float64(info.NodeCount))
}

// observeProbeLatency records one successful probe's round trip in seconds, following the
// Prometheus base-unit convention even though status reports latencyMillis.
//
// Only successful probes are observed. A failed probe's duration is a timeout, and mixing
// timeouts into the same histogram makes a quantile meaningless.
func observeProbeLatency(sc *v1beta1.SpokeCluster, latency time.Duration) {
	spokeProbeLatency.With(spokeMetricLabels(sc)).Observe(latency.Seconds())
}

// observeCredentialRefresh counts one real Materialize call, which for the aws arm is an
// sts:AssumeRole plus an eks:DescribeCluster. Cache hits never reach here, which is what
// makes the counter a measure of provider work rather than of reconcile passes.
//
// A materialize failure counts as error. An endpoint revalidation failure on a cache hit
// does not reach here at all: it revokes the gateway Secret too, but no provider was
// called, and it is reported through vela_cluster_condition_failures_total instead.
func observeCredentialRefresh(sc *v1beta1.SpokeCluster, err error) {
	result := metricResultSuccess
	if err != nil {
		result = metricResultError
	}
	spokeCredentialRefresh.WithLabelValues(sc.Namespace, sc.Name, string(sc.Spec.Credential.Type), result).Inc()
}

// forgetSpokeMetrics drops every series belonging to one SpokeCluster.
//
// Called once the finalizer is released, so a retried deletion cannot drop series while
// the object is still live. The detach counter's final increment goes with it, which is
// intended: the aggregate record of that detach lives in the Kubernetes event, which
// persists independently.
func forgetSpokeMetrics(sc *v1beta1.SpokeCluster) {
	labels := spokeMetricLabels(sc)
	spokeConnected.Delete(labels)
	spokeNodeCount.Delete(labels)
	// DeletePartialMatch for every family carrying labels beyond namespace and name.
	spokeProbeLatency.DeletePartialMatch(labels)
	spokeConnectionTransitions.DeletePartialMatch(labels)
	spokeConditionFailures.DeletePartialMatch(labels)
	spokeDetachTotal.DeletePartialMatch(labels)
	spokeCredentialRefresh.DeletePartialMatch(labels)
}
