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
	"regexp"

	"github.com/Masterminds/semver/v3"
)

// apiVersionPattern is the required format for a module line's apiVersion,
// following the Kubernetes API stability level convention (e.g. v1, v2,
// v1beta1, v1alpha2).
var apiVersionPattern = regexp.MustCompile(`^v\d+(alpha\d+|beta\d+)?$`)

// validateModuleName rejects an empty module name, naming the file it was
// read from.
func validateModuleName(name, path string) error {
	if name == "" {
		return fmt.Errorf("module in %s must not be empty", path)
	}
	return nil
}

// validateModuleVersion rejects a module version that is missing or is not
// a valid, strict semver string (major.minor.patch, no leading "v", no
// coercion of partial versions like "v1" or "1.2"), naming the file it was
// read from.
func validateModuleVersion(version, path string) error {
	if _, err := semver.StrictNewVersion(version); err != nil {
		return fmt.Errorf("version %q in %s is not a valid semver: %w", version, path, err)
	}
	return nil
}

// validateAPIVersion rejects an apiVersion that does not match
// ^v\d+(alpha\d+|beta\d+)?$, naming the file it was read from.
func validateAPIVersion(apiVersion, path string) error {
	if !apiVersionPattern.MatchString(apiVersion) {
		return fmt.Errorf("apiVersion %q in %s is invalid, must match %s", apiVersion, path, apiVersionPattern.String())
	}
	return nil
}

// validateDefinitionName rejects a rendered definition with empty
// metadata.name, naming the file it was rendered from.
func validateDefinitionName(def map[string]interface{}, path string) error {
	metadata, ok := def["metadata"].(map[string]interface{})
	if ok {
		if name, ok := metadata["name"].(string); ok && name != "" {
			return nil
		}
	}
	return fmt.Errorf("definition rendered from %s has an empty metadata.name", path)
}

// validateDefinitionsNotEmpty rejects a line whose definitions/ directory
// contains no files, naming the directory.
func validateDefinitionsNotEmpty(names []string, dirPath string) error {
	if len(names) == 0 {
		return fmt.Errorf("%s has no definitions", dirPath)
	}
	return nil
}

// validateLines rejects a module that declares no API lines.
func validateLines(lines map[string]Line) error {
	if len(lines) == 0 {
		return fmt.Errorf("module declares no API lines")
	}
	return nil
}
