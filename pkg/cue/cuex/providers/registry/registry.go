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

// Package registry provides a CueX provider that reads a file out of a
// configured addon registry.
//
// It exists so a SourceDefinition can pull a file from git without carrying a
// URL and a credential of its own: the registry is named, looked up in the
// cluster's registry ConfigMap, and read with whatever auth it was registered
// with. That keeps repository credentials a platform concern, which is the same
// separation SourceDefinitions rest on everywhere else.
package registry

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/kubevela/pkg/cue/cuex/providers"
	cuexruntime "github.com/kubevela/pkg/cue/cuex/runtime"
	"github.com/kubevela/pkg/util/runtime"

	di "github.com/oam-dev/kubevela/pkg/registry"
)

// FileReader reads one file out of a named registry.
//
// The interface lives here and the implementation does not, because reaching
// pkg/addon from this package closes a cycle: cuex -> providers/registry ->
// addon -> config -> cue/script -> cuex. The implementation is registered at
// startup by cmd/core/app/bootstrap.go, which can import both sides.
//
// It also makes the provider testable without a cluster: a test registers its
// own reader.
type FileReader interface {
	// ReadFile reads one file. An empty ref means the registry's own default.
	ReadFile(ctx context.Context, registryName, path, ref string) (string, error)
}

// ReadFileVars are the inputs to a registry file read.
type ReadFileVars struct {
	// Registry names a registry configured in this cluster.
	Registry string `json:"registry"`
	// Path is the file's path within that registry, relative to the registry's
	// own root - the same relative path `vela registry` reads addons from.
	Path string `json:"path"`
	// Ref reads at a specific branch, tag or commit. Empty means whatever the
	// registry's own URL pinned, which is the platform's default.
	Ref string `json:"ref,omitempty"`
}

// ReadFileResult is the file's contents.
type ReadFileResult struct {
	Content string `json:"content"`
}

// ReadFileParams is the params for a registry file read.
type ReadFileParams providers.Params[ReadFileVars]

// ReadFileReturns is the returns for a registry file read.
type ReadFileReturns providers.Returns[ReadFileResult]

// ReadFile fetches one file from a named registry.
//
// Errors are returned rather than swallowed: a missing registry, a missing file
// or an auth failure should fail the source's resolution loudly. A source that
// silently resolved to an empty string would be cached, and the emptiness would
// then look like data.
func ReadFile(ctx context.Context, params *ReadFileParams) (*ReadFileReturns, error) {
	in := params.Params
	if in.Registry == "" {
		return nil, fmt.Errorf("registry is required")
	}
	if in.Path == "" {
		return nil, fmt.Errorf("path is required")
	}

	reader, ok := di.Get[FileReader]()
	if !ok {
		// The binary did not wire one up. Saying so plainly beats a nil
		// dereference, and points at the one place that fixes it.
		return nil, fmt.Errorf("no registry file reader is registered; " +
			"cmd/core/app/bootstrap.go should register one")
	}

	content, err := reader.ReadFile(ctx, in.Registry, in.Path, in.Ref)
	if err != nil {
		at := ""
		if in.Ref != "" {
			at = fmt.Sprintf(" at %q", in.Ref)
		}
		return nil, fmt.Errorf("registry %q: reading %q%s: %w", in.Registry, in.Path, at, err)
	}

	return &ReadFileReturns{Returns: ReadFileResult{Content: content}}, nil
}

// ProviderName is the name a template references this provider by.
const ProviderName = "registry"

//go:embed registry.cue
var template string

// Package is the provider package, registered with the compilers that should
// offer it.
var Package = runtime.Must(cuexruntime.NewInternalPackage(ProviderName, template, map[string]cuexruntime.ProviderFn{
	"read-file": cuexruntime.GenericProviderFn[ReadFileParams, ReadFileReturns](ReadFile),
}))
