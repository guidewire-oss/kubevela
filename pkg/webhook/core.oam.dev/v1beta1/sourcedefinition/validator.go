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

package sourcedefinition

import (
	"fmt"
	"slices"
	"strings"

	"cuelang.org/go/cue/ast"
	cueliteral "cuelang.org/go/cue/literal"
	cueparser "cuelang.org/go/cue/parser"
	cuetoken "cuelang.org/go/cue/token"

	veladefinition "github.com/oam-dev/kubevela/pkg/cue/definition"
)

// ValidateSourceStorage checks the `storage:` block of a SourceDefinition template.
//
// Every SourceDefinition must declare a storage key: it names the backing Config
// cache entry and defines the sharing boundary between Applications. Without one
// the controller has no deterministic cache identity, so an absent or empty key is
// rejected here rather than papered over at resolution time.
//
// The key is usually a CUE interpolation resolved per-binding at runtime
// (`"cfg-\(context.cluster)"`), so it cannot be fully evaluated at admission. What
// is statically knowable is checked:
//   - a fully literal key is validated in full (charset and length)
//   - an interpolated key has its literal segments validated, which catches
//     characters no interpolated value could ever make legal
//
// The resolved key is validated again at resolution time, where interpolated
// values are concrete.
func ValidateSourceStorage(template string) error {
	if strings.TrimSpace(template) == "" {
		return fmt.Errorf("SourceDefinition must declare a cue template")
	}

	storage, err := topLevelField(template, "storage")
	if err != nil {
		return err
	}
	if storage == nil {
		return fmt.Errorf("SourceDefinition must declare a storage: block with a key: field")
	}

	structLit, ok := storage.Value.(*ast.StructLit)
	if !ok {
		return fmt.Errorf("storage: must be a struct declaring a key: field")
	}

	keyField := fieldByName(structLit.Elts, "key")
	if keyField == nil {
		return fmt.Errorf("storage: must declare a key: field naming the cache entry")
	}

	return validateKeyExpr(keyField.Value)
}

// ValidateSourceSchema checks the `schema:` block of a SourceDefinition template.
//
// schema: is the contract between the platform engineer and the application
// author, and it is load-bearing for the feature's security properties: admission
// validates every fromSource path against it, and the resolver validates the
// resolved output against it. Both checks are skipped when it is absent, so a
// schema-less SourceDefinition would let an application read any field of the
// resolved output with no validation at either layer. It is therefore required.
func ValidateSourceSchema(template string) error {
	schema, err := topLevelField(template, "schema")
	if err != nil {
		return err
	}
	if schema == nil {
		return fmt.Errorf("SourceDefinition must declare a schema: block")
	}
	structLit, ok := schema.Value.(*ast.StructLit)
	if !ok {
		return fmt.Errorf("schema: must be a struct declaring the fields an Application may read")
	}
	if len(structLit.Elts) == 0 {
		return fmt.Errorf("schema: must declare at least one field; an empty schema exposes nothing to fromSource")
	}
	return nil
}

// validateKeyExpr validates whatever statically-knowable text the key expression
// carries. Non-string expressions (e.g. a bare reference) are left alone: they
// cannot be checked here and are caught at resolution time.
func validateKeyExpr(expr ast.Expr) error {
	switch v := expr.(type) {
	case *ast.BasicLit:
		if v.Kind != cuetoken.STRING {
			return fmt.Errorf("storage.key must be a string, got %s", v.Kind)
		}
		key, err := cueliteral.Unquote(v.Value)
		if err != nil {
			return fmt.Errorf("storage.key is not a valid string literal: %w", err)
		}
		return veladefinition.ValidateCacheKey(key)
	case *ast.Interpolation:
		// Elts alternate literal, expression, literal, ... Only the literals are
		// knowable now; an interpolated value cannot rescue an illegal literal.
		literal := false
		for _, elt := range v.Elts {
			literal = !literal
			if !literal {
				continue
			}
			lit, ok := elt.(*ast.BasicLit)
			if !ok || lit.Kind != cuetoken.STRING {
				continue
			}
			// Interpolation fragments carry their delimiters — a fragment looks like
			// `"head\(`, `)middle\(` or `)tail"` — so strip that punctuation before
			// checking. Trim only touches the ends, so an illegal character *inside*
			// the literal text is still caught.
			text := strings.Trim(lit.Value, `"\()`)
			if bad := veladefinition.InvalidCacheKeyChars(text); bad != "" {
				return fmt.Errorf("storage.key literal segment %q contains characters not allowed in a cache key (%s); only lowercase letters, digits and '-' are permitted", text, bad)
			}
		}
	}
	return nil
}

// topLevelField returns the named top-level field of a definition template, or
// nil when it is absent.
func topLevelField(template, name string) (*ast.Field, error) {
	file, err := cueparser.ParseFile("-", template, cueparser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse cue template: %w", err)
	}
	return fieldByName(file.Decls, name), nil
}

// fieldByName finds a field with the given label among the declarations.
func fieldByName(decls []ast.Decl, name string) *ast.Field {
	for _, decl := range decls {
		field, ok := decl.(*ast.Field)
		if !ok {
			continue
		}
		label, _, err := ast.LabelName(field.Label)
		if err != nil || label != name {
			continue
		}
		return field
	}
	return nil
}

// ParseConsumableFrom reads the optional `consumableFrom:` block of a
// SourceDefinition template.
//
// It returns the declared surfaces, or nil when the block is absent - meaning
// every surface that supports fromSource. Restricting a source is opt-in: the
// common case declares nothing.
func ParseConsumableFrom(template string) ([]string, error) {
	field, err := topLevelField(template, "consumableFrom")
	if err != nil || field == nil {
		return nil, err
	}

	switch v := field.Value.(type) {
	case *ast.ListLit:
		var surfaces []string
		for _, elt := range v.Elts {
			lit, ok := elt.(*ast.BasicLit)
			if !ok {
				return nil, fmt.Errorf("consumableFrom entries must be strings")
			}
			value, err := literalString(lit)
			if err != nil {
				return nil, fmt.Errorf("consumableFrom entries must be strings: %w", err)
			}
			if !slices.Contains(veladefinition.ConsumableSurfaces, value) {
				return nil, fmt.Errorf("consumableFrom entry %q is not a surface that supports fromSource; expected one of %v",
					value, veladefinition.ConsumableSurfaces)
			}
			surfaces = append(surfaces, value)
		}
		if len(surfaces) == 0 {
			return nil, fmt.Errorf("consumableFrom must not be empty; omit it to allow every supported surface")
		}
		return surfaces, nil
	default:
		return nil, fmt.Errorf("consumableFrom must be a list of surfaces, for example [\"component\"]; omit it to allow every supported surface")
	}
}

// ValidateConsumableFrom checks that a declared consumableFrom is well-formed.
func ValidateConsumableFrom(template string) error {
	_, err := ParseConsumableFrom(template)
	return err
}

// SurfaceAllowed reports whether a source declaring the given surfaces may be
// consumed from surface. Nil surfaces means unrestricted.
func SurfaceAllowed(surfaces []string, surface string) bool {
	if len(surfaces) == 0 {
		return true
	}
	return slices.Contains(surfaces, surface)
}

func literalString(lit *ast.BasicLit) (string, error) {
	if lit.Kind != cuetoken.STRING {
		return "", fmt.Errorf("expected a string, got %s", lit.Kind)
	}
	value, err := cueliteral.Unquote(lit.Value)
	if err != nil {
		return "", fmt.Errorf("invalid string literal %s: %w", lit.Value, err)
	}
	return value, nil
}
