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

package health

import (
	"fmt"
	"strings"

	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/parser"
	"cuelang.org/go/cue/token"
)

// InheritField is the directive a snippet uses to take a concern over from the
// definition it extends.
const InheritField = "$inherit"

// SuperField is where a snippet reads what its parent decided.
const SuperField = "super"

// Snippets are one definition's status CUE.
type Snippets struct {
	Health  string
	Custom  string
	Details string
}

// inherits reports whether a snippet composes with its parent's or replaces it.
// Silence composes; `$inherit: false` takes it over.
func inherits(snippet string) bool {
	file, err := parser.ParseFile("-", snippet, parser.ParseComments)
	if err != nil {
		return true
	}
	for _, decl := range file.Decls {
		field, ok := decl.(*ast.Field)
		if !ok || labelOf(field.Label) != InheritField {
			continue
		}
		if lit, ok := field.Value.(*ast.BasicLit); ok && lit.Kind == token.FALSE {
			return false
		}
	}
	return true
}

// stripDirectives removes `$inherit` from a snippet. Every top-level field of a
// details snippet becomes a published status entry, directives included.
func stripDirectives(snippet string) string {
	var kept []string
	for _, line := range strings.Split(snippet, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), InheritField+":") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func labelOf(label ast.Label) string {
	if id, ok := label.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// superHealth is what a snippet sees of its parent's verdict.
func superHealth(healthy bool) string {
	return fmt.Sprintf("%s: isHealth: %t\n", SuperField, healthy)
}

// superMessage is what a snippet sees of its parent's status line.
func superMessage(message string) string {
	return fmt.Sprintf("%s: message: %q\n", SuperField, message)
}

// checkHealthChain evaluates a chain's health policies, root first.
//
// A level that says nothing about health takes its parent's verdict whole. One
// that states `isHealth` adds to it: both must hold. One that declares
// `$inherit: false` decides alone, and may still read `super.isHealth` to build
// on what its parent concluded.
func checkHealthChain(templateContext map[string]interface{}, snippets []Snippets, parameter interface{}) (bool, error) {
	healthy := true
	seen := false

	// Formatted once and shared: it holds the whole rendered workload.
	runtimeContextBuff, err := formatRuntimeContext(templateContext, parameter)
	if err != nil {
		return false, err
	}

	for i := len(snippets) - 1; i >= 0; i-- {
		policy := snippets[i].Health
		if strings.TrimSpace(policy) == "" {
			continue
		}
		own := stripDirectives(policy)
		if seen {
			own = superHealth(healthy) + own
		}

		got, err := checkHealthWith(runtimeContextBuff, own)
		if err != nil {
			return false, err
		}
		switch {
		case !seen:
			healthy = got
		case inherits(policy):
			healthy = healthy && got
		default:
			healthy = got
		}
		seen = true
	}
	return healthy, nil
}

// statusMessageChain evaluates a chain's custom status, root first.
//
// A level that states a message replaces its parent's, since a status line is
// one line. `super.message` is there for one that would rather extend it.
func statusMessageChain(templateContext map[string]interface{}, snippets []Snippets, parameter interface{}) (string, error) {
	message := ""

	runtimeContextBuff, err := formatRuntimeContext(templateContext, parameter)
	if err != nil {
		return "", err
	}

	for i := len(snippets) - 1; i >= 0; i-- {
		custom := snippets[i].Custom
		if strings.TrimSpace(custom) == "" {
			continue
		}
		got, err := statusMessageWith(runtimeContextBuff, superMessage(message)+stripDirectives(custom))
		if err != nil {
			return message, err
		}
		message = got
	}
	return message, nil
}

// detailsChain joins a chain's details snippets, root first.
//
// Details are a flat map of fields, so concatenation is the merge: a child's
// entries join its parent's and CUE unifies any they share. A level declaring
// `$inherit: false` starts the map again from itself.
func detailsChain(snippets []Snippets) string {
	var parts []string
	for i := len(snippets) - 1; i >= 0; i-- {
		details := snippets[i].Details
		if strings.TrimSpace(details) == "" {
			continue
		}
		if !inherits(details) {
			parts = nil
		}
		parts = append(parts, stripDirectives(details))
	}
	return strings.Join(parts, "\n")
}
