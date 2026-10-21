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
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/oam-dev/kubevela/pkg/definition"
	"github.com/oam-dev/kubevela/pkg/oam"
)

// ParseModule reads the module source tree from fsys (rooted at the module
// directory) and returns its parsed Module. It reads fsys only; it dispatches
// nothing to a cluster and performs no identity or structural validation
// beyond what validate.go covers.
func ParseModule(fsys fs.FS) (*Module, error) {
	name, err := readCUEStringField(fsys, "_module.cue", "module")
	if err != nil {
		return nil, fmt.Errorf("parse module: read module name: %w", err)
	}
	if err := validateModuleName(name, "_module.cue"); err != nil {
		return nil, fmt.Errorf("parse module: %w", err)
	}

	version, err := readCUEStringField(fsys, "_module.cue", "version")
	if err != nil {
		return nil, fmt.Errorf("parse module: read version: %w", err)
	}
	if err := validateModuleVersion(version, "_module.cue"); err != nil {
		return nil, fmt.Errorf("parse module: %w", err)
	}

	placement, err := readPlacementField(fsys, "_module.cue")
	if err != nil {
		return nil, fmt.Errorf("parse module: read placement: %w", err)
	}

	var warnings []string
	auxiliary, err := readAuxiliaryDir(fsys, "auxiliary", &warnings)
	if err != nil {
		return nil, fmt.Errorf("parse module: read auxiliary: %w", err)
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("parse module: read root: %w", err)
	}

	lines := map[string]Line{}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "v") {
			continue
		}
		line, err := parseLine(fsys, entry.Name(), &warnings)
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

	return &Module{Name: name, Version: version, Placement: placement, Warnings: warnings, Auxiliary: auxiliary, Lines: lines}, nil
}

// ParseModuleDir reads a module from a local directory. It is the
// filesystem-backed convenience over ParseModule, preserving callers that
// have a directory path rather than an fs.FS.
func ParseModuleDir(dir string) (*Module, error) {
	return ParseModule(os.DirFS(dir))
}

// parseLine reads one v<N> line directory from fsys: its apiVersion,
// placement default, auxiliary objects, and every file under definitions/ in
// sorted filename order. Non-fatal findings are appended to warnings.
func parseLine(fsys fs.FS, dir string, warnings *[]string) (*Line, error) {
	versionPath := path.Join(dir, "_version.cue")
	apiVersion, err := readCUEStringField(fsys, versionPath, "apiVersion")
	if err != nil {
		return nil, fmt.Errorf("read apiVersion: %w", err)
	}
	if err := validateAPIVersion(apiVersion, versionPath); err != nil {
		return nil, err
	}

	enabled, err := readCUEBoolField(fsys, versionPath, "enabled", true)
	if err != nil {
		return nil, fmt.Errorf("read enabled: %w", err)
	}

	placement, err := readPlacementField(fsys, versionPath)
	if err != nil {
		return nil, fmt.Errorf("read placement: %w", err)
	}

	auxiliary, err := readAuxiliaryDir(fsys, path.Join(dir, "auxiliary"), warnings)
	if err != nil {
		return nil, fmt.Errorf("read auxiliary: %w", err)
	}

	defsDir := path.Join(dir, "definitions")
	entries, err := fs.ReadDir(fsys, defsDir)
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
		defPath := path.Join(defsDir, name)
		data, err := fs.ReadFile(fsys, defPath)
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
		if err := validateObjectPlacement(rendered, defPath); err != nil {
			return nil, err
		}
		defs = append(defs, rendered)
	}

	return &Line{APIVersion: apiVersion, Enabled: enabled, Placement: placement, Auxiliary: auxiliary, Definitions: defs}, nil
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

// auxiliaryHubDir is the auxiliary/ sub-folder whose objects are stamped as
// hub-only, a shortcut for annotating each file by hand.
const auxiliaryHubDir = "hub"

// readAuxiliaryDir reads every file under dir (an auxiliary/ folder) and
// returns the objects they contain, in filename order, followed by the objects
// from its hub/ sub-folder, stamped with the hub placement annotation unless an
// object carries its own. Any other sub-folder is not read and adds a warning.
// A missing dir is not an error: it means "no auxiliary objects" for that scope.
func readAuxiliaryDir(fsys fs.FS, dir string, warnings *[]string) ([]map[string]interface{}, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	hasHub := false
	for _, entry := range entries {
		switch {
		case !entry.IsDir():
		case entry.Name() == auxiliaryHubDir:
			hasHub = true
		default:
			*warnings = append(*warnings, fmt.Sprintf("%s is not a recognized folder and is ignored; only %s/ is recognized under auxiliary/", path.Join(dir, entry.Name()), auxiliaryHubDir))
		}
	}

	objects, err := readAuxiliaryFiles(fsys, dir, entries, false, warnings)
	if err != nil {
		return nil, err
	}
	if !hasHub {
		return objects, nil
	}

	hubDir := path.Join(dir, auxiliaryHubDir)
	hubEntries, err := fs.ReadDir(fsys, hubDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range hubEntries {
		if entry.IsDir() {
			*warnings = append(*warnings, fmt.Sprintf("%s is not read; %s/ does not recurse into sub-folders", path.Join(hubDir, entry.Name()), auxiliaryHubDir))
		}
	}
	hubObjects, err := readAuxiliaryFiles(fsys, hubDir, hubEntries, true, warnings)
	if err != nil {
		return nil, err
	}
	return append(objects, hubObjects...), nil
}

// readAuxiliaryFiles decodes the files among entries (sub-folders are skipped)
// in filename order and checks each object's placement annotation. With
// stampHub set, every object without its own placement annotation is stamped as
// hub-only. An object's own annotation outranks the folder it sits in, so one
// that says runtime stays runtime and adds a warning.
func readAuxiliaryFiles(fsys fs.FS, dir string, entries []fs.DirEntry, stampHub bool, warnings *[]string) ([]map[string]interface{}, error) {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	var objects []map[string]interface{}
	for _, name := range names {
		p := path.Join(dir, name)
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		decoded, err := decodeAuxiliaryFile(name, data)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", p, err)
		}
		for _, obj := range decoded {
			if err := validateObjectPlacement(obj, p); err != nil {
				return nil, err
			}
			if !stampHub {
				continue
			}
			own, err := stampHubPlacement(obj)
			if err != nil {
				return nil, fmt.Errorf("stamp placement on object in %s: %w", p, err)
			}
			if own == oam.PlacementRuntime {
				*warnings = append(*warnings, fmt.Sprintf("%s is in %s/ but its own placement annotation is %q; the annotation wins, so it is not hub-only", p, auxiliaryHubDir, own))
			}
		}
		objects = append(objects, decoded...)
	}
	return objects, nil
}

// stampHubPlacement sets the hub placement annotation on obj's metadata,
// creating metadata and annotations when absent and keeping every other
// annotation the author wrote. An object that already carries its own placement
// annotation is left as it is, and that value is returned. The caller has
// already checked that value is hub or runtime.
func stampHubPlacement(obj map[string]interface{}) (string, error) {
	if obj == nil {
		return "", errors.New("object is empty")
	}
	metadata, err := childMap(obj, "metadata")
	if err != nil {
		return "", err
	}
	annotations, err := childMap(metadata, "annotations")
	if err != nil {
		return "", err
	}
	if own, ok := annotations[oam.AnnotationPlacement].(string); ok {
		return own, nil
	}
	annotations[oam.AnnotationPlacement] = oam.PlacementHub
	return "", nil
}

// childMap returns parent[key] as a map, creating and storing an empty one when
// the key is absent or null. A value of any other type is an error.
func childMap(parent map[string]interface{}, key string) (map[string]interface{}, error) {
	switch v := parent[key].(type) {
	case nil:
		child := map[string]interface{}{}
		parent[key] = child
		return child, nil
	case map[string]interface{}:
		return v, nil
	default:
		return nil, fmt.Errorf("%s must be a map, got %T", key, v)
	}
}

// readPlacementField returns the optional top-level placement field of the CUE
// file at p, or nil when the file does not set one. A value that is not hub or
// runtime is an error naming p.
func readPlacementField(fsys fs.FS, p string) (*string, error) {
	field, exists, err := lookupCUEField(fsys, p, "placement")
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	value, err := field.String()
	if err != nil {
		return nil, fmt.Errorf("placement in %s must be a string: %w", p, err)
	}
	if err := validatePlacement(value, p); err != nil {
		return nil, err
	}
	return &value, nil
}

// decodeAuxiliaryFile decodes one auxiliary file into the objects it
// contains: a .yaml/.yml file may hold multiple "---"-separated documents,
// each becoming its own object; a .cue file is a single object at its root.
// Any other extension is an error naming the file, not a silent skip.
func decodeAuxiliaryFile(name string, data []byte) ([]map[string]interface{}, error) {
	switch {
	case strings.HasSuffix(name, ".yaml"), strings.HasSuffix(name, ".yml"):
		return decodeYAMLDocuments(data)
	case strings.HasSuffix(name, ".cue"):
		obj, err := decodeCUEObject(data)
		if err != nil {
			return nil, err
		}
		return []map[string]interface{}{obj}, nil
	default:
		return nil, fmt.Errorf("unsupported auxiliary file type: %s", name)
	}
}

// decodeYAMLDocuments splits data on YAML document boundaries and unmarshals
// each into a generic map, skipping empty documents (a leading or trailing
// "---" produces one). This is what lets one auxiliary file hold several
// objects, the same way a plain Kubernetes manifest can.
func decodeYAMLDocuments(data []byte) ([]map[string]interface{}, error) {
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), len(data))
	var docs []map[string]interface{}
	for {
		var raw map[string]interface{}
		if err := decoder.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if len(raw) == 0 {
			continue
		}
		docs = append(docs, raw)
	}
	return docs, nil
}

// decodeCUEObject compiles a CUE auxiliary file and decodes its root value
// into a generic map. Unlike definitions/*.cue (which goes through
// definition.FromCUEString, the vela-definition wrapper), an auxiliary CUE
// file is a plain Kubernetes object at the top level: apiVersion, kind,
// metadata, and so on directly, not wrapped under a named key.
func decodeCUEObject(data []byte) (map[string]interface{}, error) {
	val := cuecontext.New().CompileBytes(data)
	if err := val.Err(); err != nil {
		return nil, err
	}
	var obj map[string]interface{}
	if err := val.Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode cue value: %w", err)
	}
	return obj, nil
}

func unmarshalYAML(data []byte) (map[string]interface{}, error) {
	obj := map[string]interface{}{}
	if err := yaml.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// lookupCUEField compiles the small identity CUE file at p in fsys and
// returns the value at fieldPath along with whether it exists, shared by
// readCUEStringField and readCUEBoolField.
func lookupCUEField(fsys fs.FS, p, fieldPath string) (cue.Value, bool, error) {
	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return cue.Value{}, false, err
	}
	val := cuecontext.New().CompileBytes(data)
	if err := val.Err(); err != nil {
		return cue.Value{}, false, err
	}
	field := val.LookupPath(cue.ParsePath(fieldPath))
	return field, field.Exists(), nil
}

// readCUEStringField returns the string value at fieldPath (e.g. "module",
// "version", "apiVersion"); a missing field is an error since these are
// required identity fields.
func readCUEStringField(fsys fs.FS, p, fieldPath string) (string, error) {
	field, exists, err := lookupCUEField(fsys, p, fieldPath)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("field %s not found in %s", fieldPath, p)
	}
	return field.String()
}

// readCUEBoolField returns the bool value at fieldPath. A missing field
// yields def, so an optional switch like "enabled" can default without every
// module having to declare it.
func readCUEBoolField(fsys fs.FS, p, fieldPath string, def bool) (bool, error) {
	field, exists, err := lookupCUEField(fsys, p, fieldPath)
	if err != nil {
		return false, err
	}
	if !exists {
		return def, nil
	}
	return field.Bool()
}
