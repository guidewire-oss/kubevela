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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

var (
	spokeConnectionTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spokecluster_connection_transitions_total",
		Help: "SpokeCluster connection state transitions observed by the controller.",
	}, []string{"to"})

	spokeConditionFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spokecluster_condition_failures_total",
		Help: "SpokeCluster condition transitions to False (credential or register failures).",
	}, []string{"condition", "reason"})

	spokeDetachTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spokecluster_detach_total",
		Help: "SpokeCluster detach (delete) outcomes.",
	}, []string{"result"})
)

func init() {
	metrics.Registry.MustRegister(spokeConnectionTransitions, spokeConditionFailures, spokeDetachTotal)
	// Eagerly create the stable label sets so /metrics exposes the families even
	// before the first transition (CounterVec is otherwise lazy).
	for _, to := range []string{"Connected", "Disconnected"} {
		spokeConnectionTransitions.WithLabelValues(to)
	}
	// Only the (condition, reason) pairs emitStatusEvents can increment.
	spokeConditionFailures.WithLabelValues(v1beta1.SpokeClusterConditionCredentialValid, reasonMaterializeFailed)
	spokeConditionFailures.WithLabelValues(v1beta1.SpokeClusterConditionCredentialValid, reasonNoProvider)
	spokeConditionFailures.WithLabelValues(v1beta1.SpokeClusterConditionCredentialValid, reasonSpecInvalid)
	spokeConditionFailures.WithLabelValues(v1beta1.SpokeClusterConditionRegistered, reasonRegisterFailed)
	for _, result := range []string{"success", "error"} {
		spokeDetachTotal.WithLabelValues(result)
	}
}
