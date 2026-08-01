/*
Copyright 2025 The KubeVela Authors.

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
	"context"
	"fmt"

	"github.com/kubevela/pkg/util/singleton"
	"k8s.io/klog/v2"

	"github.com/oam-dev/kubevela/pkg/addon"
	"github.com/oam-dev/kubevela/pkg/config"
	cuexregistry "github.com/oam-dev/kubevela/pkg/cue/cuex/providers/registry"
	cuexvelaconfig "github.com/oam-dev/kubevela/pkg/cue/cuex/providers/velaconfig"
	"github.com/oam-dev/kubevela/pkg/registry"
)

// bootstrapProviderRegistry registers framework-level providers that need
// to be available globally. This is called early in prepareRun before
// controllers are initialized.
//
// The provider registry is a fallback mechanism for breaking import cycles
// that block development. Providers registered here enable immediate feature
// work while longer-term refactoring efforts can be planned.
//
// Best practices when adding a provider:
// 1. Document which packages have the cycle (in comments below)
// 2. Define narrow, focused interfaces (< 5 methods)
// 3. Consider opportunities for future refactoring to eliminate the cycle
// 4. Prefer constructor injection for new code without cycles
//
// See pkg/registry/README.md for feature overview and pkg/registry package docs for guidelines.
// addonRegistryFileReader reads a file from an addon registry configured in the
// cluster.
//
// Registries are stored in a ConfigMap, so this works from the controller, and
// each backend - GitHub, Gitee, GitLab, OSS - already knows how to read a file
// with whatever credential the registry was registered with. That is the point
// of going through registries rather than taking a URL and a token per source:
// repository credentials stay a platform concern.
type addonRegistryFileReader struct{}

func (addonRegistryFileReader) ReadFile(ctx context.Context, registryName, path, ref string) (string, error) {
	store := addon.NewRegistryDataStore(singleton.KubeClient.Get())
	reg, err := store.GetRegistry(ctx, registryName)
	if err != nil {
		return "", err
	}

	var opts []addon.ReaderOption
	if ref != "" {
		opts = append(opts, addon.WithRef(ref))
	}
	reader, err := reg.BuildReader(opts...)
	if err != nil {
		return "", fmt.Errorf("registry cannot be read: %w", err)
	}
	return reader.ReadFile(path)
}

// velaConfigReader reads a Config through pkg/config, which also refuses one
// marked sensitive - a guard worth inheriting rather than reimplementing in the
// provider.
type velaConfigReader struct{}

func (velaConfigReader) ReadConfig(ctx context.Context, namespace, name string) (*cuexvelaconfig.ReadResult, error) {
	factory := config.NewConfigFactory(singleton.KubeClient.Get())

	// ReadConfig first, and deliberately: it is the call that returns
	// ErrSensitiveConfig. GetConfig below merely blanks a sensitive Config's
	// properties and secret data, so on its own it would let a sensitive
	// Config's template and output references through.
	props, err := factory.ReadConfig(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	// The second read is for the template and the output references, which
	// ReadConfig does not carry. withStatus is false - distribution status
	// describes where a Config has been replicated to, which is an operational
	// concern rather than data a workload should be shaping itself around.
	cfg, err := factory.GetConfig(ctx, namespace, name, false)
	if err != nil {
		return nil, err
	}

	outputs := make([]cuexvelaconfig.ObjectRef, 0, len(cfg.ObjectReferences))
	for _, ref := range cfg.ObjectReferences {
		outputs = append(outputs, cuexvelaconfig.ObjectRef{
			APIVersion: ref.APIVersion,
			Kind:       ref.Kind,
			Name:       ref.Name,
			Namespace:  ref.Namespace,
		})
	}

	return &cuexvelaconfig.ReadResult{
		Properties: props,
		Template: cuexvelaconfig.TemplateRef{
			Name:      cfg.Template.Name,
			Namespace: cfg.Template.Namespace,
		},
		Outputs: outputs,
	}, nil
}

func bootstrapProviderRegistry() {
	klog.V(2).InfoS("Bootstrapping provider registry")

	// ────────────────────────────────────────────────────────────────────
	// Add providers below following this pattern:
	// ────────────────────────────────────────────────────────────────────
	//
	// ProviderInterface - Brief description
	// Cycle: pkg/foo ↔ pkg/bar (explain the circular dependency)
	// Note: Consider refactoring to extract shared interfaces
	// registry.RegisterAs[ProviderInterface](implementation)

	// cuexregistry.FileReader - lets a SourceDefinition read a file from a
	// registry the platform has configured.
	// Cycle: pkg/cue/cuex -> providers/registry -> pkg/addon -> pkg/config ->
	//        pkg/cue/script -> pkg/cue/cuex
	// The provider declares the interface; only this file can see both it and
	// pkg/addon, so the implementation is wired here.
	registry.RegisterAs[cuexregistry.FileReader](addonRegistryFileReader{})

	// velaconfig.Reader - lets a SourceDefinition read a Config's properties.
	// Cycle: pkg/cue/cuex -> providers/velaconfig -> pkg/config ->
	//        pkg/cue/script -> pkg/cue/cuex
	registry.RegisterAs[cuexvelaconfig.Reader](velaConfigReader{})

	klog.V(2).InfoS("Provider registry bootstrap complete")
}
