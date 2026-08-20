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

package addon

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	pkgaddon "github.com/oam-dev/kubevela/pkg/addon"
)

// TestVersionMismatchIsDistinguishable is the contract the webhook relies on:
// checkAddonVersionMeetRequired reports both "requirement evaluated and not met"
// and "requirement could not be evaluated" through a plain error, and only the
// first is a reason to deny. This webhook runs with failurePolicy: Fail, so
// denying on a transient discovery-API or vela-core lookup failure would block
// Application applies.
func TestVersionMismatchIsDistinguishable(t *testing.T) {
	cases := map[string]struct {
		err        error
		wantDenial bool
	}{
		"a real mismatch is denied": {
			err:        fmt.Errorf("%w: the kubernetes version v1.31.5 require: <=v1.3.0", pkgaddon.ErrVersionMismatch),
			wantDenial: true,
		},
		"a wrapped mismatch is still denied": {
			err:        fmt.Errorf("checking addon foo: %w", fmt.Errorf("%w: mismatch", pkgaddon.ErrVersionMismatch)),
			wantDenial: true,
		},
		"a discovery-API failure fails open": {
			err:        errors.New("Get \"https://10.0.0.1:443/version\": dial tcp: i/o timeout"),
			wantDenial: false,
		},
		"a vela-core image lookup failure fails open": {
			err:        errors.New("cannot get vela core deployment: etcdserver: request timed out"),
			wantDenial: false,
		},
		"a malformed constraint fails open rather than denying": {
			err:        errors.New("improper constraint: <= 1.3.0"),
			wantDenial: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.wantDenial, errors.Is(tc.err, pkgaddon.ErrVersionMismatch))
		})
	}
}

// TestDefaultCompatCheckerFailsOpenWithoutCluster covers the production checker
// with no Kubernetes client wired up: every resolve path fails, and the result
// must be nil (allow) rather than a denial.
func TestDefaultCompatCheckerFailsOpenWithoutCluster(t *testing.T) {
	h := &ValidatingHandler{}
	assert.Nil(t, h.defaultCompatChecker(context.Background(), "some-addon", "", ""),
		"an unreachable registry or cluster must never produce a denial")
}
