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

package resourcekeeper

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
)

// PruneComponentResources deletes the resources a component used to render and
// no longer does.
//
// Garbage collection is ResourceTracker-level: an individual resource stops
// being desired only when its whole tracker becomes history, which happens when
// a new ApplicationRevision is created. Every ordinary change to desired state
// goes through a revision, which is what makes that sufficient. Source-driven
// refresh does not - re-dispatching on an external value change without minting
// a revision is the point of it - so a component that renders a different set
// than it did last reconcile leaves the difference tracked and running forever.
//
// The obvious case is a resource whose name derives from source data: the
// rename dispatches a second object rather than updating the first, and nothing
// reaps the original. Any shrink in the rendered set leaks the same way.
//
// This is deliberately NOT folded into Dispatch. Multi-stage component apply
// dispatches subsets of a component's manifests across stages, so treating any
// dispatch as "this is now the complete set" would reap the stages that have
// not run yet.
//
// keep must be the component's *complete* rendered set across every placement.
// Anything attributed to the component and absent from it is deleted, so a
// caller that has only part of the picture must not call this at all - see the
// all-or-nothing rule in refreshSourceDrivenComponents.
func (h *resourceKeeper) PruneComponentResources(ctx context.Context, component string,
	keep []*unstructured.Unstructured) ([]v1beta1.ManagedResource, error) {
	if component == "" {
		return nil, nil
	}
	rt, err := h.getCurrentRT(ctx)
	if err != nil {
		return nil, err
	}
	if rt == nil {
		return nil, nil
	}

	// Keyed without the cluster: the same component renders the same object name
	// in every cluster it is placed in, and where a source makes the name vary
	// by cluster, every variant is in the union and every variant is kept. That
	// errs towards keeping a resource that could have been reaped, which is the
	// safe direction.
	kept := make(map[string]struct{}, len(keep))
	for _, u := range keep {
		if u != nil {
			kept[resourceKey(u.GetAPIVersion(), u.GetKind(), u.GetNamespace(), u.GetName())] = struct{}{}
		}
	}

	var stale []*unstructured.Unstructured
	var pruned []v1beta1.ManagedResource
	for _, mr := range rt.Spec.ManagedResources {
		if mr.Deleted || mr.Component != component {
			continue
		}
		if _, ok := kept[resourceKey(mr.APIVersion, mr.Kind, mr.Namespace, mr.Name)]; ok {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(mr.APIVersion)
		u.SetKind(mr.Kind)
		u.SetNamespace(mr.Namespace)
		u.SetName(mr.Name)
		stale = append(stale, u)
		pruned = append(pruned, mr)
	}
	if len(stale) == 0 {
		return nil, nil
	}
	// Through Delete rather than a direct client call, so the tracker entry is
	// marked and any garbage-collect policy on the resource still applies - a
	// resource the user asked to keep on app update is kept here too.
	if err := h.Delete(ctx, stale); err != nil {
		return nil, err
	}
	return pruned, nil
}

func resourceKey(apiVersion, kind, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s/%s", apiVersion, kind, namespace, name)
}
