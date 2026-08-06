/*
Copyright 2021 The KubeVela Authors.

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

// Package service fetches a module from a registry and parses it into a
// module.Module, server-side, for the type: module render path. It reuses the
// pkg/addon transport (git reader, OCI Helm-chart client) and the Registry
// model; it does not reuse the addon parsing/packaging layer.
package service

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/chart/loader"

	"github.com/oam-dev/kubevela/pkg/addon"
	"github.com/oam-dev/kubevela/pkg/module"
)

// ModuleRegistryStore resolves module registries by name. Its two methods match
// addon.RegistryDataStore exactly (value Registry, not pointer), so GWCP-106679's
// store — an addon.RegistryDataStore over the separate vela-module-registry
// ConfigMap — satisfies this directly with no adapter. GetRegistry already loads
// the token from its secret (loadTokenFromSecret), so fetch does no auth itself.
type ModuleRegistryStore interface {
	GetRegistry(ctx context.Context, name string) (addon.Registry, error)
	ListRegistries(ctx context.Context) ([]addon.Registry, error)
}

// Service fetches modules. Its reader/puller seams are wired to the real addon
// transport by NewService and overridden by tests.
type Service struct {
	store     ModuleRegistryStore
	newReader func(reg *addon.Registry) (addon.AsyncReader, error)
	pullChart ociChartPuller
}

// NewService wires the real addon transport.
func NewService(store ModuleRegistryStore) *Service {
	return &Service{
		store:     store,
		newReader: buildModuleReader,
		pullChart: pullModuleChart,
	}
}

// buildModuleReader builds the reader for a module registry, pointed at its
// modules root (the source Path); ListAddonMeta then keys each module by name.
// It reuses the addon transport (reg.BuildReader), returning the addon.AsyncReader
// readerFS consumes.
func buildModuleReader(reg *addon.Registry) (addon.AsyncReader, error) {
	return reg.BuildReader()
}

// FetchModule resolves the registry, fetches the module's files into an fs.FS,
// and parses them. An empty registry name resolves the sole registry.
func (s *Service) FetchModule(ctx context.Context, registry, moduleName string) (*module.Module, error) {
	reg, err := s.resolve(ctx, registry)
	if err != nil {
		return nil, err
	}
	fsys, err := s.sourceFS(ctx, reg, moduleName)
	if err != nil {
		return nil, err
	}
	mod, err := module.ParseModule(fsys)
	if err != nil {
		return nil, fmt.Errorf("registry %q, module %q: %w", reg.Name, moduleName, err)
	}
	return mod, nil
}

// resolve returns the named registry, or the sole registry when name is empty.
// This default policy (empty -> sole) is the one bit of resolution addon has no
// equivalent for; GetRegistry/ListRegistries and token loading are all reused.
func (s *Service) resolve(ctx context.Context, name string) (*addon.Registry, error) {
	if name != "" {
		reg, err := s.store.GetRegistry(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("resolve module registry %q: %w", name, err)
		}
		return &reg, nil
	}
	regs, err := s.store.ListRegistries(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve default module registry: %w", err)
	}
	switch len(regs) {
	case 1:
		return &regs[0], nil
	case 0:
		return nil, fmt.Errorf("no module registry configured; specify one with --registry")
	default:
		names := make([]string, 0, len(regs))
		for _, r := range regs {
			names = append(names, r.Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("multiple module registries (%s); specify one with --registry", strings.Join(names, ", "))
	}
}

// sourceFS dispatches on the registry source type and returns the module tree
// as an fs.FS. Both branches converge on readerFS: git supplies the live reader,
// OCI (Task 4) supplies a MemoryReader over the pulled chart. readerFS errors are
// wrapped with the registry name so a failing Application status is actionable.
func (s *Service) sourceFS(ctx context.Context, reg *addon.Registry, moduleName string) (fs.FS, error) {
	switch {
	case reg.OCI != nil:
		return s.ociChartFS(ctx, reg, moduleName)
	case reg.Git != nil || reg.Gitee != nil || reg.Gitlab != nil || reg.OSS != nil:
		reader, err := s.newReader(reg)
		if err != nil {
			return nil, fmt.Errorf("registry %q: build reader: %w", reg.Name, err)
		}
		fsys, err := readerFS(reader, moduleName)
		if err != nil {
			return nil, fmt.Errorf("registry %q: %w", reg.Name, err)
		}
		return fsys, nil
	default:
		return nil, fmt.Errorf("registry %q has no supported module source", reg.Name)
	}
}

// readerFS is the single source->tree adapter. It reads the module's files from
// any addon.AsyncReader and assembles a mapFS keyed module-root-relative. It uses
// RelativePath (not the raw item path) because that is the reader-agnostic path
// both the live git reader and MemoryReader accept for ReadFile: the git reader
// strips its configured base, and MemoryReader returns "<module>/<rel>". Both
// forms start with "<module>/", which readerFS then strips.
func readerFS(r addon.AsyncReader, moduleName string) (fs.FS, error) {
	metas, err := r.ListAddonMeta()
	if err != nil {
		return nil, fmt.Errorf("list modules: %w", err)
	}
	meta, ok := metas[moduleName]
	if !ok {
		return nil, fmt.Errorf("module %q not found in registry", moduleName)
	}
	prefix := moduleName + "/"
	files := mapFS{}
	for _, item := range meta.Items {
		if item.GetType() != addon.FileType {
			continue
		}
		readPath := r.RelativePath(item)
		rel := strings.TrimPrefix(readPath, prefix)
		if rel == "" || rel == readPath {
			continue // not under the module root
		}
		content, err := r.ReadFile(readPath)
		if err != nil {
			return nil, fmt.Errorf("module %q: read %s: %w", moduleName, readPath, err)
		}
		files[rel] = []byte(content)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("module %q is empty", moduleName)
	}
	return files, nil
}

type ociChartPuller func(ctx context.Context, reg *addon.Registry, moduleName string) ([]*loader.BufferedFile, error)

// pullModuleChart pulls the module's Helm-chart OCI artifact (same semantics as
// vela addon push) and returns its buffered files, paths prefixed by the chart
// (module) name. It reuses addon's pull verbatim; ociChartFS wraps the files in a
// MemoryReader and runs readerFS. Version "" resolves the highest semver tag.
func pullModuleChart(ctx context.Context, reg *addon.Registry, moduleName string) ([]*loader.BufferedFile, error) {
	buffered, err := addon.PullOCIChartFiles(ctx, *reg, moduleName, "")
	if err != nil {
		return nil, fmt.Errorf("module %q: pull OCI chart: %w", moduleName, err)
	}
	return buffered, nil
}

// ociChartFS pulls the module's Helm chart and reuses readerFS by wrapping the
// buffered files in addon.MemoryReader (itself an addon.AsyncReader). No new
// adapter — the OCI blob just becomes a reader.
func (s *Service) ociChartFS(ctx context.Context, reg *addon.Registry, moduleName string) (fs.FS, error) {
	bufs, err := s.pullChart(ctx, reg, moduleName)
	if err != nil {
		return nil, fmt.Errorf("registry %q: %w", reg.Name, err)
	}
	fsys, err := readerFS(&addon.MemoryReader{Name: moduleName, Files: bufs}, moduleName)
	if err != nil {
		return nil, fmt.Errorf("registry %q: %w", reg.Name, err)
	}
	return fsys, nil
}
