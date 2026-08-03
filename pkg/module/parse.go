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

package module

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	"sigs.k8s.io/yaml"

	"github.com/oam-dev/kubevela/pkg/definition"
)

// ParseModule reads the module source tree at dir and returns its parsed
// Module. It reads the filesystem only; it dispatches nothing to a cluster
// and performs no identity or structural validation (see validate.go).
func ParseModule(dir string) (*Module, error) {
	modulePath := filepath.Join(dir, "_module.cue")
	name, err := readCUEStringField(modulePath, "module")
	if err != nil {
		return nil, fmt.Errorf("parse module: read module name: %w", err)
	}
	if err := validateModuleName(name, modulePath); err != nil {
		return nil, fmt.Errorf("parse module: %w", err)
	}

	version, err := readCUEStringField(modulePath, "version")
	if err != nil {
		return nil, fmt.Errorf("parse module: read version: %w", err)
	}
	if err := validateModuleVersion(version, modulePath); err != nil {
		return nil, fmt.Errorf("parse module: %w", err)
	}

	xrd, err := readOptionalYAMLFile(filepath.Join(dir, "auxiliary", "xrd.yaml"))
	if err != nil {
		return nil, fmt.Errorf("parse module: read xrd: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("parse module: read %s: %w", dir, err)
	}

	lines := map[string]Line{}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "v") {
			continue
		}
		line, err := parseLine(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("parse line %s: %w", entry.Name(), err)
		}
		if _, exists := lines[line.APIVersion]; exists {
			return nil, fmt.Errorf("parse module: duplicate line %q (from directory %s)", line.APIVersion, entry.Name())
		}
		lines[line.APIVersion] = *line
	}

	if err := validateLines(lines); err != nil {
		return nil, fmt.Errorf("parse module: %w", err)
	}

	return &Module{Name: name, Version: version, XRD: xrd, Lines: lines}, nil
}

// parseLine reads one v<N> line directory: its apiVersion, Composition,
// and every file under definitions/ in sorted filename order.
func parseLine(dir string) (*Line, error) {
	versionPath := filepath.Join(dir, "_version.cue")
	apiVersion, err := readCUEStringField(versionPath, "apiVersion")
	if err != nil {
		return nil, fmt.Errorf("read apiVersion: %w", err)
	}
	if err := validateAPIVersion(apiVersion, versionPath); err != nil {
		return nil, err
	}

	composition, err := readOptionalYAMLFile(filepath.Join(dir, "auxiliary", "composition.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read composition: %w", err)
	}

	defsDir := filepath.Join(dir, "definitions")
	entries, err := os.ReadDir(defsDir)
	if err != nil {
		return nil, fmt.Errorf("read definitions dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if err := validateDefinitionsNotEmpty(names, defsDir); err != nil {
		return nil, err
	}

	defs := make([]map[string]interface{}, 0, len(names))
	for _, name := range names {
		defPath := filepath.Join(defsDir, name)
		data, err := os.ReadFile(defPath)
		if err != nil {
			return nil, fmt.Errorf("read definition %s: %w", name, err)
		}
		rendered, err := renderDefinition(name, data)
		if err != nil {
			return nil, fmt.Errorf("render definition %s: %w", name, err)
		}
		if err := validateDefinitionName(rendered, defPath); err != nil {
			return nil, err
		}
		defs = append(defs, rendered)
	}

	return &Line{APIVersion: apiVersion, Composition: composition, Definitions: defs}, nil
}

// renderDefinition compiles a single definition file into its unstructured
// object: .cue through KubeVela's own definition renderer, .yaml/.yml by
// unmarshaling; any other extension is rejected.
func renderDefinition(name string, data []byte) (map[string]interface{}, error) {
	switch {
	case strings.HasSuffix(name, ".cue"):
		def := &definition.Definition{}
		if err := def.FromCUEString(string(data), nil); err != nil {
			return nil, err
		}
		return def.Object, nil
	case strings.HasSuffix(name, ".yaml"), strings.HasSuffix(name, ".yml"):
		return unmarshalYAML(data)
	default:
		return nil, fmt.Errorf("unsupported definition file type: %s", name)
	}
}

// readOptionalYAMLFile reads and unmarshals a YAML file into a generic map.
// A missing file is not an error: it returns a nil map. Any other read or
// unmarshal failure (permissions, malformed YAML) is still returned as an
// error, since only absence is treated as "not provided."
func readOptionalYAMLFile(path string) (map[string]interface{}, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return unmarshalYAML(data)
}

func unmarshalYAML(data []byte) (map[string]interface{}, error) {
	obj := map[string]interface{}{}
	if err := yaml.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// readCUEStringField compiles the small identity CUE file at path and
// returns the string value at fieldPath (e.g. "module.name", "apiVersion").
func readCUEStringField(path, fieldPath string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	val := cuecontext.New().CompileBytes(data)
	if err := val.Err(); err != nil {
		return "", err
	}
	field := val.LookupPath(cue.ParsePath(fieldPath))
	if !field.Exists() {
		return "", fmt.Errorf("field %s not found in %s", fieldPath, path)
	}
	return field.String()
}
