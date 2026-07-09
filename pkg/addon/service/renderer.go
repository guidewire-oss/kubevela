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

// Package service provides a render-only addon service: it resolves an addon
// from the registry and renders its Application plus auxiliaries, without
// dispatching anything to the cluster.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubevela/pkg/util/singleton"

	pkgaddon "github.com/oam-dev/kubevela/pkg/addon"
	"github.com/oam-dev/kubevela/pkg/addon/service/api"
	"github.com/oam-dev/kubevela/pkg/utils/common"
)

type rendererImpl struct {
	cli    client.Client
	config *rest.Config

	// cache is the in-process half of "resolve once": it maps a cacheKey built
	// from every render input to the rendered *api.AddonResult, so repeat
	// requests skip registry I/O and CUE rendering. The durable pin is the
	// rendered manifests captured in the ApplicationRevision by the existing
	// Application controller. No invalidation is needed: the key already
	// includes every input, so a version/property change is simply a new key.
	cache sync.Map

	// resolveFn is a seam for tests: it defaults to r.resolveAndRender and is
	// only overridden by unit tests that count invocations. Production always
	// uses the default.
	resolveFn func(ctx context.Context, req api.AddonRequest) (*api.AddonResult, error)
}

// NewRenderer builds a render-only addon service. It reads the Kubernetes
// client and rest config from the kubevela-pkg singletons at call time, so no
// startup injection is required.
func NewRenderer() api.Renderer {
	return &rendererImpl{}
}

// init registers the default renderer so a blank import of this package
// (from cmd/core) wires the addon CueX provider, instead of injecting it in
// server.go.
func init() { api.SetDefaultRenderer(NewRenderer()) }

// client returns the injected client (tests) or the shared singleton (production).
func (r *rendererImpl) client() client.Client {
	if r.cli != nil {
		return r.cli
	}
	return singleton.KubeClient.Get()
}

// restConfig returns the injected config (tests) or the shared singleton (production).
func (r *rendererImpl) restConfig() *rest.Config {
	if r.config != nil {
		return r.config
	}
	return singleton.KubeConfig.Get()
}

// cacheKey builds the resolve-once cache key from every input that can change
// the rendered output, including SkipVersionValidate so a validated request and
// a skipped request never alias to the same cached result.
func cacheKey(req api.AddonRequest) string {
	return fmt.Sprintf("%s|%s|%s|%t|%s", req.Name, req.Version, req.Registry, req.SkipVersionValidate, hashProperties(req.Properties))
}

// hashProperties returns a stable SHA-256 hex digest of the properties map.
// json.Marshal sorts map keys, so the marshaled bytes are canonical and the
// digest is order-independent.
func hashProperties(properties map[string]interface{}) string {
	b, err := json.Marshal(properties)
	if err != nil {
		// A non-marshalable value cannot produce a stable key; fall back to a
		// sentinel that is distinct from any real digest so such requests are
		// never served a stale cache entry.
		return "unhashable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RenderAddon resolves and renders an addon, caching the result by cacheKey so
// identical requests avoid repeat registry I/O and rendering.
func (r *rendererImpl) RenderAddon(ctx context.Context, req api.AddonRequest) (*api.AddonResult, error) {
	key := cacheKey(req)
	if cached, ok := r.cache.Load(key); ok {
		return cached.(*api.AddonResult), nil
	}

	resolve := r.resolveFn
	if resolve == nil {
		resolve = r.resolveAndRender
	}
	res, err := resolve(ctx, req)
	if err != nil {
		return nil, err
	}
	r.cache.Store(key, res)
	return res, nil
}

func (r *rendererImpl) resolveAndRender(ctx context.Context, req api.AddonRequest) (*api.AddonResult, error) {
	regs := []string{}
	if req.Registry != "" {
		regs = []string{req.Registry}
	}
	pkgs, err := pkgaddon.FindAddonPackagesDetailFromRegistry(ctx, r.client(), []string{req.Name}, regs)
	if err != nil {
		return nil, fmt.Errorf("addon %q not found in registries %v: %w", req.Name, regs, err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("addon %q not found in registries %v", req.Name, regs)
	}
	whole := pkgs[0]
	installPkg := &whole.InstallPackage
	registryName := whole.RegistryName

	// FindAddonPackagesDetailFromRegistry loads a concrete package (the latest for
	// versioned registries). If the caller pinned an exact version that differs,
	// fetch that specific version.
	resolved := installPkg.Version
	if req.Version != "" && req.Version != resolved {
		exact, err := r.fetchExactVersion(ctx, registryName, req.Name, req.Version)
		if err != nil {
			return nil, fmt.Errorf("fetch addon %q version %q: %w", req.Name, req.Version, err)
		}
		installPkg = exact
		resolved = exact.Version
	}

	if !req.SkipVersionValidate {
		if err := r.validateSystemRequirements(ctx, req.Name, installPkg); err != nil {
			return nil, err
		}
	}

	app, aux, err := pkgaddon.RenderApp(ctx, installPkg, r.client(), req.Properties)
	if err != nil {
		return nil, fmt.Errorf("render addon %q: %w", req.Name, err)
	}

	resources := make([]map[string]interface{}, 0, len(aux))
	for _, o := range aux {
		resources = append(resources, o.Object)
	}

	auxiliaries, err := r.renderAuxiliaries(ctx, installPkg, req.Properties)
	if err != nil {
		return nil, fmt.Errorf("render auxiliaries for addon %q: %w", req.Name, err)
	}
	resources = append(resources, auxiliaries...)

	appMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(app)
	if err != nil {
		return nil, err
	}
	appMap["apiVersion"] = "core.oam.dev/v1beta1"
	appMap["kind"] = "Application"

	// Strip server-populated fields that break CUE completion when a manifest
	// becomes a component output: metadata.creationTimestamp marshals to null,
	// which CUE treats as an incomplete value ("_"), and status is not part of
	// the desired state.
	sanitizeManifest(appMap)
	for _, res := range resources {
		sanitizeManifest(res)
	}

	return &api.AddonResult{
		ResolvedVersion: resolved,
		Registry:        registryName,
		Application:     appMap,
		Resources:       resources,
	}, nil
}

// sanitizeManifest removes the root status subresource and every
// metadata.creationTimestamp (recursively, including nested objects such as the
// resources inside a k8s-objects component) so the manifest completes cleanly
// as CUE.
func sanitizeManifest(m map[string]interface{}) {
	delete(m, "status")
	stripCreationTimestamp(m)
}

func stripCreationTimestamp(v interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		if meta, ok := t["metadata"].(map[string]interface{}); ok {
			delete(meta, "creationTimestamp")
		}
		for _, val := range t {
			stripCreationTimestamp(val)
		}
	case []interface{}:
		for _, item := range t {
			stripCreationTimestamp(item)
		}
	}
}

// validateSystemRequirements checks the addon's SystemRequirements against the
// running environment. When the rest config is nil (e.g. in unit tests) the
// check is skipped with a logged note, since a discovery client cannot be built.
func (r *rendererImpl) validateSystemRequirements(ctx context.Context, name string, installPkg *pkgaddon.InstallPackage) error {
	cfg := r.restConfig()
	if cfg == nil {
		klog.InfoS("skipping addon system requirement check: no rest config available", "addon", name)
		return nil
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build discovery client for addon %q version check: %w", name, err)
	}
	if err := pkgaddon.ValidateSystemRequirements(ctx, installPkg.SystemRequirements, r.client(), dc); err != nil {
		return fmt.Errorf("addon %q does not meet system requirements: %w", name, err)
	}
	return nil
}

// fetchExactVersion resolves a specific addon version from the named registry.
func (r *rendererImpl) fetchExactVersion(ctx context.Context, registryName, addonName, version string) (*pkgaddon.InstallPackage, error) {
	ds := pkgaddon.NewRegistryDataStore(r.client())
	reg, err := ds.GetRegistry(ctx, registryName)
	if err != nil {
		return nil, fmt.Errorf("get registry %q: %w", registryName, err)
	}

	if pkgaddon.IsVersionRegistry(reg) {
		vr := pkgaddon.BuildVersionedRegistry(reg.Name, reg.Helm.URL, &common.HTTPOption{
			Username:        reg.Helm.Username,
			Password:        reg.Helm.Password,
			InsecureSkipTLS: reg.Helm.InsecureSkipTLS,
		})
		return vr.GetAddonInstallPackage(ctx, addonName, version)
	}

	metas, err := reg.ListAddonMeta()
	if err != nil {
		return nil, err
	}
	meta, ok := metas[addonName]
	if !ok {
		return nil, fmt.Errorf("addon %q not found in registry %q", addonName, registryName)
	}
	uiData, err := reg.GetUIData(&meta, pkgaddon.UIMetaOptions)
	if err != nil {
		return nil, err
	}
	return reg.GetInstallPackage(&meta, uiData)
}

// renderAuxiliaries renders the definition, config-template, schema, view and
// args-secret objects the dispatcher normally applies alongside the addon
// Application, converting each to a generic map for CUE consumption.
func (r *rendererImpl) renderAuxiliaries(ctx context.Context, installPkg *pkgaddon.InstallPackage, properties map[string]interface{}) ([]map[string]interface{}, error) {
	var objs []*unstructured.Unstructured

	defs, err := pkgaddon.RenderDefinitions(installPkg, r.restConfig())
	if err != nil {
		return nil, err
	}
	objs = append(objs, defs...)

	configTemplates, err := pkgaddon.RenderConfigTemplates(ctx, installPkg, r.client())
	if err != nil {
		return nil, err
	}
	objs = append(objs, configTemplates...)

	schemas, err := pkgaddon.RenderDefinitionSchema(installPkg)
	if err != nil {
		return nil, err
	}
	objs = append(objs, schemas...)

	views, err := pkgaddon.RenderViews(ctx, installPkg)
	if err != nil {
		return nil, err
	}
	objs = append(objs, views...)

	if secret := pkgaddon.RenderArgsSecret(installPkg, properties); secret != nil {
		objs = append(objs, secret)
	}

	out := make([]map[string]interface{}, 0, len(objs))
	for _, o := range objs {
		if o == nil {
			continue
		}
		out = append(out, o.Object)
	}
	return out, nil
}
