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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/multicluster"
)

// probe asks the spoke's API server for /apis through the cluster-gateway proxy and
// measures the round trip. /apis requires authentication on typical clusters (including
// EKS and k3s), so a successful probe proves the registered credential still works, not
// merely that the apiserver answers anonymous /healthz.
//
// The measured latency is returned for discovery to record as clusterInfo.latencyMillis, so
// the probe is where that number comes from rather than a second round trip.
//
// A 401 already forces a credential refresh: reconcileConnect evicts the cached credential
// on that one status, so the next pass remints rather than reusing a rejected token. Mapping
// a 403 to its own condition reason is still a later refinement; today it is described in the
// probe-failure message (see describeProbeFailure) but shares reasonProbeFailed.
func (r *Reconciler) probe(ctx context.Context, sc *v1beta1.SpokeCluster) (time.Duration, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout(sc))
	defer cancel()

	// RequestRawK8sAPIForCluster mutates the config it is handed (GroupVersion and
	// NegotiatedSerializer) and its deferred restore nils those fields rather than
	// returning them to their previous values. Its non-local path also builds a client with
	// NewForConfigOrDie, which panics on a config another goroutine has just nilled out. So
	// every probe gets its own copy: with concurrent reconciles, sharing the hub config
	// would take the whole manager down.
	cfg := rest.CopyConfig(r.Config)

	start := time.Now()
	if _, err := multicluster.RequestRawK8sAPIForCluster(probeCtx, "apis", sc.Name, cfg); err != nil {
		// Returned unwrapped: describeProbeFailure names the spoke and endpoint when it
		// renders this for status, and wrapping here only duplicated the cluster name.
		return 0, err
	}
	return time.Since(start), nil
}

// describeProbeFailure renders a probe error for status.conditions.
//
// The raw error is unhelpful on its own: RequestRawK8sAPIForCluster reports the hub's own
// cluster-gateway proxy URL (an in-cluster service address), so an operator reading it goes
// looking at the wrong machine, and every distinct failure collapses into the same
// "context deadline exceeded". This names the endpoint that is actually unreachable, the
// timeout that applied, and what the failure looks like, while keeping the original error
// on the end so nothing is lost.
func describeProbeFailure(sc *v1beta1.SpokeCluster, endpoint string, err error) string {
	if endpoint == "" {
		endpoint = "unknown endpoint"
	}

	var cause string
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		cause = fmt.Sprintf("no response within the %s probe timeout; the spoke API server is unreachable, "+
			"still starting, or blocked by network policy", probeTimeout(sc))
	case apierrors.IsUnauthorized(err):
		cause = "the spoke rejected the credential (401); it may have been revoked or expired"
	case apierrors.IsForbidden(err):
		cause = "the spoke refused the authenticated probe (403); check the RBAC bound to this credential"
	case apierrors.IsNotFound(err):
		cause = "cluster-gateway has no route for this spoke; check the gateway Secret in " +
			multicluster.ClusterGatewaySecretNamespace
	case isGatewayServerError(err):
		// What a deleted or torn-down spoke actually produces: the gateway is reachable and
		// answers, but its own dial to the spoke fails, so the hub gets a 500 rather than a
		// timeout. Without this branch the most common real failure reads as "did not succeed".
		cause = "cluster-gateway could not reach the spoke API server; the cluster may be deleted, " +
			"stopped, or no longer routable from the hub"
	case isConnectionRefused(err):
		cause = "connection refused; the endpoint is resolvable but nothing is listening"
	case isNoSuchHost(err):
		cause = "the endpoint hostname does not resolve from the hub"
	case isTLSFailure(err):
		cause = "TLS verification failed; the endpoint host must match the spoke certificate SAN " +
			"(a k3d serverlb address will not)"
	default:
		cause = "the request through cluster-gateway did not succeed"
	}

	return fmt.Sprintf("spoke %q unreachable at %s: %s (underlying error: %v)", sc.Name, endpoint, cause, err)
}

// isGatewayServerError reports whether the API returned 5xx. apierrors.IsInternalError only
// matches reason=InternalError, and cluster-gateway's proxy failures surface as a 500 whose
// reason is empty, so the status code is what has to be checked.
func isGatewayServerError(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return false
	}
	return status.Status().Code >= http.StatusInternalServerError
}

func isConnectionRefused(err error) bool {
	return strings.Contains(err.Error(), "connection refused")
}

func isNoSuchHost(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no such host") || strings.Contains(msg, "server misbehaving")
}

func isTLSFailure(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "x509") || strings.Contains(msg, "tls:") ||
		strings.Contains(msg, "certificate is valid for")
}

// probeTimeout bounds a single probe. The schema defaults spec.probeTimeoutSeconds to 10 and
// the mutating webhook mirrors that, so the guard below only ever applies to an object
// written before the default landed, or built directly in a test.
func probeTimeout(sc *v1beta1.SpokeCluster) time.Duration {
	if sc.Spec.ProbeTimeoutSeconds > 0 {
		return time.Duration(sc.Spec.ProbeTimeoutSeconds) * time.Second
	}
	return defaultProbeTimeout
}
