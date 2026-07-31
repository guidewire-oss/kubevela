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

// KeyInputsField is the generated sibling of storage.key: the values the resolver
// folds into the identity hash. It is recorded rather than re-derived so that
// inference stays a build-time concern, and so the resolver hashes exactly what
// the template reads rather than, say, every label on the object.
const KeyInputsField = "keyInputs"

// RulesVersionAnnotation records the readable name of the same policy. The hash
// above is what validation looks up; this is for whoever is reading the object
// and wants to know which policy applied without computing anything.
const RulesVersionAnnotation = "definition.oam.dev/cache-key-rules-version"

// Stamp writes the inferred cache key into a SourceDefinition template and
// returns it alongside the hash of the rules used.
//
// A key already present is accepted when it matches what inference produces and
// rejected when it does not. Accepting a match is what lets a definition
// round-trip: `vela def get` emits the stored template, key included, so
// rejecting any key at all would break get, edit, apply. Rejecting a mismatch
// rather than overwriting it keeps the author's belief about how their source is
// cached from being silently corrected.
//
// So there is one rule for both the authored CUE and the applied object: the key
// equals what inference produces, and is written for you when absent.
//
// It is idempotent - the generated key is not itself read when inferring - so
// re-stamping an already stamped template yields the same bytes. That matters
// because a re-apply of an unchanged definition must not show a diff.
func Stamp(definitionName, template string) (string, *Rules, error) {
	rules, err := LoadRules()
	if err != nil {
		return "", nil, err
	}

	dims, err := Infer(template, rules)
	if err != nil {
		return "", nil, err
	}
	expr, err := KeyExpression(definitionName, dims, rules)
	if err != nil {
		return "", nil, err
	}

	file, err := cueparser.ParseFile("-", template, cueparser.ParseComments)
	if err != nil {
		return "", nil, fmt.Errorf("parse cue template: %w", err)
	}
	if existing, ok := existingStorageKey(file); ok && existing != expr {
		return "", nil, fmt.Errorf("storage.key is computed from the context this template reads, and %s "+
			"does not match: expected %s. Correct it, or leave it out and it will be written for you",
			existing, expr)
	}
	setStorageKey(file, expr)
	setKeyInputs(file, KeyInputs(dims))

	out, err := cueformat.Node(file)
	if err != nil {
		return "", nil, fmt.Errorf("format stamped template: %w", err)
	}
	return string(out), rules, nil
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

// existingStorageKey returns the key already written in the template, as source
// text, and whether there was one.
func existingStorageKey(file *ast.File) (string, bool) {
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
			if name, _, err := ast.LabelName(f.Label); err != nil || name != keyField {
				continue
			}
			out, err := cueformat.Node(f.Value)
			if err != nil {
				return "", false
			}
			return string(out), true
		}
	}
	return "", false
}

// setKeyInputs records the values the identity hash covers, replacing any list
// already present so regeneration stays idempotent.
func setKeyInputs(file *ast.File, inputs []string) {
	elts := make([]ast.Expr, 0, len(inputs))
	for _, in := range inputs {
		elts = append(elts, ast.NewString(in))
	}
	value := ast.NewList(elts...)

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
			if name, _, err := ast.LabelName(f.Label); err == nil && name == KeyInputsField {
				f.Value = value
				return
			}
		}
		st.Elts = append(st.Elts, &ast.Field{Label: ast.NewIdent(KeyInputsField), Value: value})
		return
	}
}

func newKeyField(value ast.Expr) *ast.Field {
	return &ast.Field{Label: ast.NewIdent(keyField), Value: value}
}

// Verify re-derives the cache key from a stored template and checks the key the
// template carries matches it.
//
// This is the admission-side half of the contract. The CLI writes the key, but
// nothing stops a stored object being edited or written by hand, so the key is
// re-derived rather than trusted.
//
// rulesHash names the policy that produced the key, taken from the object's
// annotation. It is used in preference to the current rules so that changing
// inference does not invalidate definitions already generated and committed -
// which matters because GitOps re-applies them continuously. An empty hash means
// the object was not produced by `vela def`, and the current rules apply: the key
// still has to be right, just by today's policy.
func Verify(definitionName, template, rulesHash string) error {
	rules, err := rulesFor(rulesHash)
	if err != nil {
		return err
	}

	dims, err := Infer(template, rules)
	if err != nil {
		return err
	}
	expected, err := KeyExpression(definitionName, dims, rules)
	if err != nil {
		return err
	}

	file, err := cueparser.ParseFile("-", template, cueparser.ParseComments)
	if err != nil {
		return fmt.Errorf("parse cue template: %w", err)
	}
	existing, ok := existingStorageKey(file)
	if !ok {
		return fmt.Errorf("storage.key is missing; it is computed from the context this template reads "+
			"and should be %s. Apply the definition with `vela def apply` and it will be written for you",
			expected)
	}
	if existing != expected {
		return fmt.Errorf("storage.key is computed from the context this template reads, and %s does not "+
			"match: expected %s", existing, expected)
	}
	return nil
}

// rulesFor loads the named policy, or the current one when nothing is named.
func rulesFor(rulesHash string) (*Rules, error) {
	if rulesHash == "" {
		return LoadRules()
	}
	return LoadRulesByHash(rulesHash)
}
