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

package appfile

import (
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
)

// Policies fall into three kinds, and property expressions behave differently in
// each. The distinction was previously implicit - a switch in parsePolicies, a
// scope lookup elsewhere - which is why all three shared one set of rules and got
// the narrowest of them.
//
//   - built-in: topology, override, garbage-collect and friends. Their properties
//     are read straight off af.Policies by a provider; nothing renders them, so
//     there is no resolver and no source. Expressions are substituted while the
//     appfile is built.
//   - rendered: any other type, which is a PolicyDefinition with a CUE template.
//     It renders through NewWorkloadAbstractEngine exactly as a component does,
//     so a source resolves there.
//   - application-scoped: renders before the appfile exists, so there is no
//     spec.sources[] to resolve against at all.

// builtinPolicyTypes are the policy types KubeVela consumes directly rather than
// rendering.
//
// Taken from the switch in parsePolicies. Note that parsePoliciesFromRevision
// carries a *different* list - it omits replication - so an Application parsed
// from a revision classifies that one policy differently from the same
// Application parsed fresh. That inconsistency predates this file and is not
// resolved here; the fresh-parse list is used because it is the one that governs
// admission, which is where the expression rules are enforced. TestBuiltinPolicyTypesMatchParser
// pins the agreement so the two cannot drift further apart unnoticed.
var builtinPolicyTypes = map[string]bool{
	v1alpha1.GarbageCollectPolicyType: true,
	v1alpha1.ApplyOncePolicyType:      true,
	v1alpha1.SharedResourcePolicyType: true,
	v1alpha1.TakeOverPolicyType:       true,
	v1alpha1.ReadOnlyPolicyType:       true,
	v1alpha1.ResourceUpdatePolicyType: true,
	v1alpha1.EnvBindingPolicyType:     true,
	v1alpha1.TopologyPolicyType:       true,
	v1alpha1.OverridePolicyType:       true,
	v1alpha1.DebugPolicyType:          true,
	v1alpha1.ReplicationPolicyType:    true,
}

// IsBuiltinPolicyType reports a policy KubeVela consumes rather than renders.
//
// Such a policy has no render, so its properties are substituted while the
// appfile is built, from what the Appfile carries - and it can never read a
// source, because there is no resolver on that path.
func IsBuiltinPolicyType(policyType string) bool {
	return builtinPolicyTypes[policyType]
}
