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

	"cuelang.org/go/cue/ast"
	cueformat "cuelang.org/go/cue/format"
	cueparser "cuelang.org/go/cue/parser"
	cuetoken "cuelang.org/go/cue/token"
)

// RulesAnnotation records which inference policy generated a definition's cache
// key. Validation loads the rules with this hash rather than the current ones, so
// changing inference never invalidates a definition already generated.
const RulesAnnotation = "definition.oam.dev/cache-key-rules"

// Stamp writes the inferred cache key into a SourceDefinition template and
// returns it alongside the hash of the rules used.
//
// It is idempotent: an existing key is replaced rather than appended to, and the
// generated key is not itself read when inferring, so re-stamping an already
// stamped template yields the same bytes. That matters because a re-apply of an
// unchanged definition must not show a diff.
func Stamp(definitionName, template string) (string, string, error) {
	rules, err := LoadRules()
	if err != nil {
		return "", "", err
	}

	dims, err := Infer(template, rules)
	if err != nil {
		return "", "", err
	}
	expr, err := KeyExpression(definitionName, dims)
	if err != nil {
		return "", "", err
	}

	file, err := cueparser.ParseFile("-", template, cueparser.ParseComments)
	if err != nil {
		return "", "", fmt.Errorf("parse cue template: %w", err)
	}
	setStorageKey(file, expr)

	out, err := cueformat.Node(file)
	if err != nil {
		return "", "", fmt.Errorf("format stamped template: %w", err)
	}
	return string(out), rules.Hash, nil
}

// setStorageKey sets storage.key to expr, creating the storage block if needed
// and leaving any other fields in it - storageTTL and onStaleFailure are authored.
func setStorageKey(file *ast.File, expr string) {
	keyValue := ast.NewLit(cuetoken.STRING, expr)

	for _, decl := range file.Decls {
		field, ok := decl.(*ast.Field)
		if !ok {
			continue
		}
		if name, _, err := ast.LabelName(field.Label); err != nil || name != storageField {
			continue
		}
		st, ok := field.Value.(*ast.StructLit)
		if !ok {
			continue
		}
		for _, elt := range st.Elts {
			f, ok := elt.(*ast.Field)
			if !ok {
				continue
			}
			if name, _, err := ast.LabelName(f.Label); err == nil && name == keyField {
				f.Value = keyValue
				return
			}
		}
		// storage: exists without a key; put the key first so it reads as the
		// identity of the block rather than an afterthought.
		st.Elts = append([]ast.Decl{newKeyField(keyValue)}, st.Elts...)
		return
	}

	// No storage: block at all.
	file.Decls = append(file.Decls, &ast.Field{
		Label: ast.NewIdent(storageField),
		Value: &ast.StructLit{Elts: []ast.Decl{newKeyField(keyValue)}},
	})
}

func newKeyField(value ast.Expr) *ast.Field {
	return &ast.Field{Label: ast.NewIdent(keyField), Value: value}
}
