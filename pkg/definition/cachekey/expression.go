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

package cachekey

import (
	"fmt"
	"strings"

	"cuelang.org/go/cue"

	veladefinition "github.com/oam-dev/kubevela/pkg/cue/definition"
)

// KeyExpression renders the CUE expression that becomes storage.key.
//
// The result is a complete string literal, ready to place after `key:`. It
// interpolates the same context the template already reads, so the controller
// evaluates it exactly as it would an authored key - inference never runs at
// reconcile time.
//
// The resolver appends the properties hash, so this is the context portion of
// the cache identity rather than the whole of it. Length is not checked here:
// the expression is static text, and how long the resolved key runs is only
// knowable once the interpolation has values.
func KeyExpression(definitionName string, dims []Dimension) (string, error) {
	if strings.TrimSpace(definitionName) == "" {
		return "", fmt.Errorf("definition name is empty; a cache key needs it as a prefix")
	}
	// The prefix is the one part of the key that is fixed at generation time, so
	// it is also the one part that can be checked now.
	if bad := veladefinition.InvalidCacheKeyChars(definitionName); bad != "" {
		return "", fmt.Errorf("definition name %q contains characters not allowed in a cache key (%s); "+
			"only lowercase letters, digits and '-' are permitted", definitionName, bad)
	}

	var b strings.Builder
	b.WriteString(`"`)
	b.WriteString(definitionName)
	for _, d := range dims {
		b.WriteString(`-\(`)
		b.WriteString(contextIdent)
		b.WriteString(".")
		b.WriteString(d.Field)
		if d.Index != "" {
			// A quoted index inside an interpolation is legal CUE, and keeps the
			// expression readable rather than hiding the label behind an alias.
			b.WriteString(`["`)
			b.WriteString(d.Index)
			b.WriteString(`"]`)
		}
		b.WriteString(`)`)
	}
	b.WriteString(`"`)
	return b.String(), nil
}

// cuePathKey is the path the expression tests evaluate.
func cuePathKey() cue.Path { return cue.ParsePath("key") }
