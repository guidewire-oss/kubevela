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

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/kubevela/pkg/util/singleton"

	"github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/module"
	"github.com/oam-dev/kubevela/pkg/module/service/api"
	"github.com/oam-dev/kubevela/pkg/oam"
)

// rendererImpl fetches a module and renders its owned Application.
type rendererImpl struct {
	// fetchFn is a seam for tests. Production leaves it nil and the real
	// FetchModule is built per call, so no Kubernetes client is required at
	// construction time (init runs before the singletons are populated).
	fetchFn func(ctx context.Context, registry, moduleName, version string) (*module.Module, error)
}

// NewRenderer builds the module render service. It reads the Kubernetes client
// from the kubevela-pkg singleton at call time, not at construction, so a blank
// import can wire it during package init.
func NewRenderer() api.Renderer { return &rendererImpl{} }

// init registers the default renderer so a blank import of this package from
// cmd/core wires the module CueX provider, instead of injecting it in server.go.
func init() { api.SetDefaultRenderer(NewRenderer()) }

// fetch resolves the module through the injected seam (tests) or the real
// registry-backed FetchModule (production).
func (r *rendererImpl) fetch(ctx context.Context, registry, moduleName, version string) (*module.Module, error) {
	if r.fetchFn != nil {
		return r.fetchFn(ctx, registry, moduleName, version)
	}
	return NewService(module.NewStore(singleton.KubeClient.Get())).FetchModule(ctx, registry, moduleName, version)
}

// RenderModule fetches the named module and renders its owned Application.
func (r *rendererImpl) RenderModule(ctx context.Context, req api.ModuleRequest) (*api.ModuleResult, error) {
	mod, err := r.fetch(ctx, req.Registry, req.Module, req.Version)
	if err != nil {
		return nil, err
	}
	app, err := RenderApplication(mod, req.Namespace)
	if err != nil {
		return nil, err
	}
	return &api.ModuleResult{Application: app}, nil
}

// RenderApplication builds the module's owned Application. It is pure: given a
// parsed Module it touches no cluster and no registry, which is what lets the
// whole tier layout be unit-tested against a fixture.
func RenderApplication(mod *module.Module, namespace string) (map[string]interface{}, error) {
	if mod == nil || mod.Name == "" {
		return nil, fmt.Errorf("render module: module has no name")
	}
	if namespace == "" {
		namespace = types.DefaultKubeVelaNS
	}

	comps := []interface{}{}

	// The module-level auxiliary tiers are module-wide and gate every line
	// beneath them. Auxiliary is split by readiness: any CompositeResourceDefinition
	// gates on Established, everything else installs and is healthy once applied.
	established, rest := splitAuxiliaryByReadiness(mod.Auxiliary)
	dep := ""
	if len(established) > 0 {
		dep = mod.Name + "-aux-established"
		comps = append(comps, readyTier(dep, established, "Established", ""))
	}
	if len(rest) > 0 {
		auxTier := mod.Name + "-aux"
		// The rest group has no readiness signal to wait on,
		// so an empty condition type means healthy once applied.
		comps = append(comps, readyTier(auxTier, rest, "", dep))
		dep = auxTier
	}
	moduleDep := dep

	for _, apiVersion := range enabledLines(mod) {
		line := mod.Lines[apiVersion]

		// Each line hangs off the module-level auxiliary, not off the previous
		// line: lines are siblings, so v2 must not wait on v1.
		dep := moduleDep
		lineEstablished, lineRest := splitAuxiliaryByReadiness(line.Auxiliary)
		if len(lineEstablished) > 0 {
			tier := fmt.Sprintf("%s-%s-aux-established", mod.Name, apiVersion)
			comps = append(comps, readyTier(tier, lineEstablished, "Established", dep))
			dep = tier
		}
		if len(lineRest) > 0 {
			tier := fmt.Sprintf("%s-%s-aux", mod.Name, apiVersion)
			// Same readiness carrier as the module-level rest tier: no real
			// condition to wait on today, healthy once applied.
			comps = append(comps, readyTier(tier, lineRest, "", dep))
			dep = tier
		}

		if len(line.Definitions) == 0 {
			continue
		}
		defs := make([]interface{}, 0, len(line.Definitions))
		for _, def := range line.Definitions {
			defs = append(defs, stampIdentity(def, mod.Name, apiVersion))
		}
		comps = append(comps, objectsTier(fmt.Sprintf("%s-%s-defs", mod.Name, apiVersion), defs, dep))
	}

	return map[string]interface{}{
		"apiVersion": "core.oam.dev/v1beta1",
		"kind":       "Application",
		"metadata": map[string]interface{}{
			"name":      "module-" + mod.Name,
			"namespace": namespace,
			"labels": map[string]interface{}{
				types.LabelDefinitionModule: mod.Name,
			},
			"annotations": map[string]interface{}{
				// mod.Version is parsed from the fetched module's own _module.cue,
				// so it is always the concrete tag that was actually fetched --
				// including when the fetch request asked for "latest".
				types.AnnoDefinitionModuleVersion: mod.Version,
			},
		},
		"spec": map[string]interface{}{"components": comps},
	}, nil
}

// enabledLines returns the API versions to install: every line whose Enabled is
// true, sorted for a deterministic component order (and therefore a stable
// ApplicationRevision). Sorting is lexical, so v10 precedes v2; order between
// lines carries no meaning because lines are siblings under the XRD.
func enabledLines(mod *module.Module) []string {
	out := make([]string, 0, len(mod.Lines))
	for v, line := range mod.Lines {
		if !line.Enabled {
			continue
		}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// XRDKind is the  CompositeResourceDefinition kind. An
// auxiliary object of this kind is the one readiness signal
// the renderer knows how to wait on: everything else installs
// and is healthy once applied.
const XRDKind = "CompositeResourceDefinition"

// splitAuxiliaryByReadiness partitions a scope's auxiliary objects into the
// ones that gate on Established (CompositeResourceDefinitions) and everything
// else, preserving each group's original order.
func splitAuxiliaryByReadiness(aux []map[string]interface{}) (established, rest []interface{}) {
	for _, obj := range aux {
		if kind, _ := obj["kind"].(string); kind == XRDKind {
			established = append(established, obj)
			continue
		}
		rest = append(rest, obj)
	}
	return established, rest
}

// readyTier wraps objects in k8s-objects with a readyConditionType, so the next
// tier waits on a real status condition rather than on mere application. An empty
// readyConditionType means "healthy once applied" (plain k8s-objects behavior).
func readyTier(name string, objects []interface{}, readyConditionType, dependsOn string) map[string]interface{} {
	c := map[string]interface{}{
		"name": name,
		"type": "k8s-objects",
		"properties": map[string]interface{}{
			"objects":            objects,
			"readyConditionType": readyConditionType,
		},
	}
	if dependsOn != "" {
		c["dependsOn"] = []interface{}{dependsOn}
	}
	return c
}

// objectsTier wraps objects in a plain k8s-objects component (no readyConditionType,
// so healthy once applied). Nothing gates on the definitions tier.
func objectsTier(name string, objects []interface{}, dependsOn string) map[string]interface{} {
	c := map[string]interface{}{
		"name":       name,
		"type":       "k8s-objects",
		"properties": map[string]interface{}{"objects": objects},
	}
	if dependsOn != "" {
		c["dependsOn"] = []interface{}{dependsOn}
	}
	return c
}

// maxObjectNameLen is the Kubernetes limit for a metadata.name.
const maxObjectNameLen = 253

// maxLabelValueLen is the Kubernetes limit for a label value.
const maxLabelValueLen = 63

// stampIdentity returns a copy of def carrying its module identity: the
// {module}-{apiVersion}-{name} object name, the definition identity labels, the
// full-name annotation, and the spec identity fields. It copies rather than
// mutates because the parsed Module is shared and may be cached.
func stampIdentity(def map[string]interface{}, moduleName, apiVersion string) map[string]interface{} {
	out := deepCopyMap(def)

	meta, _ := out["metadata"].(map[string]interface{})
	if meta == nil {
		meta = map[string]interface{}{}
		out["metadata"] = meta
	}
	shortName, _ := meta["name"].(string)

	fullName := fmt.Sprintf("%s-%s-%s", moduleName, apiVersion, shortName)
	meta["name"] = truncateName(fullName)

	labels, _ := meta["labels"].(map[string]interface{})
	if labels == nil {
		labels = map[string]interface{}{}
		meta["labels"] = labels
	}
	labels[types.LabelDefinitionModule] = moduleName
	labels[types.LabelDefinitionModuleAPIVersion] = apiVersion
	// The definition name can be up to the object-name limit, but a label value
	// caps at 63 chars, so bound it; the untruncated name lives on the full-name
	// annotation below.
	labels[types.LabelDefinitionName] = truncateWithHash(shortName, maxLabelValueLen)
	labels[oam.LabelAddonName] = moduleName

	annos, _ := meta["annotations"].(map[string]interface{})
	if annos == nil {
		annos = map[string]interface{}{}
		meta["annotations"] = annos
	}
	annos[types.AnnoDefinitionModuleFullName] = fullName

	spec, _ := out["spec"].(map[string]interface{})
	if spec == nil {
		spec = map[string]interface{}{}
		out["spec"] = spec
	}
	spec["module"] = moduleName
	spec["apiVersion"] = apiVersion

	return out
}

// truncateName keeps name within the Kubernetes object-name limit, appending a
// stable 8-char digest of the full name so two long names that share a prefix
// still get distinct objects. The untruncated name lives on the full-name
// annotation.
func truncateName(name string) string {
	return truncateWithHash(name, maxObjectNameLen)
}

// truncateWithHash keeps s within max bytes, appending a stable 8-char digest of
// the full value so two long values sharing a prefix stay distinct.
func truncateWithHash(s string, max int) string {
	if len(s) <= max {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	return s[:max-len(suffix)] + suffix
}

// deepCopyMap copies nested maps and slices so stamping never writes through to
// the fetched Module.
func deepCopyMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return deepCopyMap(t)
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	case []map[string]interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = deepCopyMap(e)
		}
		return out
	default:
		return v
	}
}
