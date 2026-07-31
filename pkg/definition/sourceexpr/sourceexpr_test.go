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

package sourceexpr

import (
	"strings"
	"testing"

	"cuelang.org/go/cue"

	"github.com/oam-dev/kubevela/pkg/definition/cachekey"
)

var demoSchemas = map[string]string{
	"my-source": `{region: string, count: int, ratio: float, enabled: bool}`,
	"other":     `{name: string}`,
}

func TestParse(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantExprs []string
		wantWhole bool
		wantErr   string
	}{
		{
			name: "a plain value is untouched",
			raw:  "just-a-string",
		},
		{
			name:      "a whole value is one expression",
			raw:       `$(source["my-source"].region)`,
			wantExprs: []string{`source["my-source"].region`},
			wantWhole: true,
		},
		{
			name:      "an embedded expression keeps its surrounding text",
			raw:       `prefix-$(source["my-source"].region)-suffix`,
			wantExprs: []string{`source["my-source"].region`},
		},
		{
			name:      "several expressions in one value",
			raw:       `$(source["my-source"].region)/$(source["other"].name)`,
			wantExprs: []string{`source["my-source"].region`, `source["other"].name`},
		},
		{
			// Without an escape a value that genuinely contains the delimiter
			// could not be written at all.
			name: "the delimiter can be escaped",
			raw:  `literal $$(not-an-expression)`,
		},
		{
			name:      "parens inside the expression are balanced",
			raw:       `$(strings.Join(["a"], ")"))`,
			wantExprs: []string{`strings.Join(["a"], ")")`},
			wantWhole: true,
		},
		{
			name:    "an unterminated expression is an error, not a literal",
			raw:     `$(source["my-source"].region`,
			wantErr: "unterminated",
		},
		{
			name:    "an empty expression is an error",
			raw:     `$()`,
			wantErr: "empty expression",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected an error mentioning %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var exprs []string
			for _, f := range got.Fragments {
				if f.IsExpr() {
					exprs = append(exprs, f.Expr)
				}
			}
			if len(exprs) != len(tc.wantExprs) {
				t.Fatalf("expected expressions %v, got %v", tc.wantExprs, exprs)
			}
			for i := range exprs {
				if exprs[i] != tc.wantExprs[i] {
					t.Fatalf("expected expressions %v, got %v", tc.wantExprs, exprs)
				}
			}
			if got.Whole() != tc.wantWhole {
				t.Errorf("Whole() = %v, want %v", got.Whole(), tc.wantWhole)
			}
		})
	}
}

// The grammar gate is what makes sentinel typing sound, so its boundary is the
// contract - not a stylistic restriction.
func TestValidate(t *testing.T) {
	accept := []string{
		`source["my-source"].region`,
		`source["my-source"].region + "-cluster"`,
		`"\(source["my-source"].region)-\(source["my-source"].count)"`,
		`source["my-source"].count * 2`,
	}
	for _, expr := range accept {
		if err := Validate(expr); err != nil {
			t.Errorf("expected %q to be accepted, got: %v", expr, err)
		}
	}

	reject := []struct {
		expr string
		why  string
	}{
		// Each of these has a result type that depends on a value rather than on
		// operand types, so typing it against a sentinel would be a guess.
		{`[if source["my-source"].count > 5 {"big"}, "small"][0]`, "indexing"},
		{`source["my-source"].region | "default"`, "disjunction"},
		{`source["my-source"].count > 5`, "comparison"},
		// Sandbox: an expression reaches `source` and `context`, nothing else.
		{`parameter.image`, "unknown identifier"},
		{`strings.ToUpper("x")`, "unknown identifier"},
	}
	for _, tc := range reject {
		err := Validate(tc.expr)
		if err == nil {
			t.Errorf("expected %q to be rejected (%s)", tc.expr, tc.why)
		}
	}
}

// Admission needs to know which paths an expression reads: to validate each
// against the source's schema, to order the bindings, and to propagate
// +sensitive taint to the result.
func TestReferences(t *testing.T) {
	refs, err := References(`"\(source["my-source"].region)-\(source["other"].name)" + source["my-source"].region`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"source.my-source.region", "source.other.name"}
	if len(refs) != len(want) {
		t.Fatalf("expected %v, got %v", want, refs)
	}
	for i := range want {
		if refs[i].String() != want[i] {
			t.Fatalf("expected %v, got %v", want, refs)
		}
	}
}

// The heart of the spike: the same expression is typed at admission and
// evaluated at render, so a mismatch is refused before the Application exists.
func TestTypeOf(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		want    cue.Kind
		wantErr string
	}{
		{name: "a bare reference", expr: `source["my-source"].region`, want: cue.StringKind},
		{name: "concatenation", expr: `source["my-source"].region + "-cluster"`, want: cue.StringKind},
		{name: "interpolation", expr: `"\(source["my-source"].region)/\(source["my-source"].count)"`, want: cue.StringKind},
		{name: "integer arithmetic", expr: `source["my-source"].count * 2`, want: cue.IntKind},
		{
			// CUE's / is float division, so the inferred type says float rather
			// than pretending the result is an int.
			name: "division yields a float",
			expr: `source["my-source"].count / 2`,
			want: cue.FloatKind,
		},
		{name: "a bool field", expr: `source["my-source"].enabled`, want: cue.BoolKind},
		{
			name:    "a type mismatch is caught",
			expr:    `source["my-source"].count + "-cluster"`,
			wantErr: "invalid operands",
		},
		{
			name:    "a field outside the schema is caught",
			expr:    `source["my-source"].undeclared`,
			wantErr: "not declared in the source's schema",
		},
		{
			name:    "an unknown source is caught",
			expr:    `source["nonexistent"].field`,
			wantErr: "undefined field",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TypeOf(tc.expr, demoSchemas)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected an error mentioning %q, got %v (kind %s)", tc.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected kind %s, got %s", tc.want, got)
			}
		})
	}
}

// An int sentinel of 0 would fail admission with a division-by-zero that could
// never happen at render. The sentinel is 1 for exactly this case.
func TestTypeOfDoesNotInventDivisionByZero(t *testing.T) {
	if _, err := TypeOf(`100 / source["my-source"].count`, demoSchemas); err != nil {
		t.Fatalf("a division by a schema int must type cleanly, got: %v", err)
	}
}

func TestEval(t *testing.T) {
	resolved := map[string]map[string]interface{}{
		"my-source": {"region": "eu-west", "count": 3, "enabled": true},
		"other":     {"name": "checkout"},
	}

	cases := []struct {
		name string
		raw  string
		want interface{}
	}{
		{name: "a plain value is returned unchanged", raw: "literal", want: "literal"},
		{name: "concatenation", raw: `$(source["my-source"].region + "-cluster")`, want: "eu-west-cluster"},
		{name: "embedded in text", raw: `https://$(source["other"].name).example.com`, want: "https://checkout.example.com"},
		{
			// The reason Whole() exists: a lone expression keeps its type, so a
			// parameter declared int receives a number rather than the string
			// "3". CUE decodes an integer as int64.
			name: "a whole value keeps its type",
			raw:  `$(source["my-source"].count)`,
			want: int64(3),
		},
		{
			name: "an embedded number becomes text",
			raw:  `replicas-$(source["my-source"].count)`,
			want: "replicas-3",
		},
		{name: "an escaped delimiter is literal", raw: `$$(literal)`, want: "$(literal)"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Eval(tc.raw, resolved, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %#v (%T), got %#v (%T)", tc.want, tc.want, got, got)
			}
		})
	}
}

// Admission and render must agree, or the type check is theatre: whatever TypeOf
// promised is what Eval has to produce.
func TestTypeOfAgreesWithEval(t *testing.T) {
	resolved := map[string]map[string]interface{}{
		"my-source": {"region": "eu-west", "count": 3, "ratio": 1.5, "enabled": true},
		"other":     {"name": "checkout"},
	}

	for _, expr := range []string{
		`source["my-source"].region`,
		`source["my-source"].region + "-cluster"`,
		`source["my-source"].count * 2`,
		`"\(source["my-source"].region)/\(source["my-source"].count)"`,
		`source["my-source"].enabled`,
	} {
		kind, err := TypeOf(expr, demoSchemas)
		if err != nil {
			t.Fatalf("%s: typing: %v", expr, err)
		}
		value, err := Eval("$("+expr+")", resolved, nil)
		if err != nil {
			t.Fatalf("%s: evaluating: %v", expr, err)
		}

		var actual cue.Kind
		switch v := value.(type) {
		case string:
			actual = cue.StringKind
		case bool:
			actual = cue.BoolKind
		case int:
			actual = cue.IntKind
		case int64:
			actual = cue.IntKind
		case float64:
			actual = cue.FloatKind
			if v == float64(int64(v)) {
				actual = cue.IntKind
			}
		default:
			t.Fatalf("%s: unexpected result type %T", expr, value)
		}

		if actual != kind {
			t.Errorf("%s: admission promised %s, render produced %s (%#v)", expr, kind, actual, value)
		}
	}
}

// The sharpest edge in this syntax, and the reason it needs its own diagnostic.
//
// A source name containing a hyphen cannot be reached with dots: CUE parses
// `source.my-source.region` as `(source.my) - (source.region)`. Both halves are
// legal CUE, so nothing complains until the evaluator reports "undefined field:
// my", which tells the author nothing about what they did wrong.
func TestHyphenatedNameHazard(t *testing.T) {
	err := Validate(`source.my-source.region`)
	if err == nil {
		t.Fatal("a hyphenated name written with dots is subtraction and must be rejected")
	}
	for _, want := range []string{"subtraction", `source["<name>"]`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must point at the fix; expected it to mention %q, got: %v", want, err)
		}
	}

	// The bracket form is the fix, and it types normally.
	kind, err := TypeOf(`source["my-source"].region`, demoSchemas)
	if err != nil {
		t.Fatalf("the bracket form must work: %v", err)
	}
	if kind != cue.StringKind {
		t.Fatalf("expected string, got %s", kind)
	}
}

// Subtraction between two genuinely numeric source fields is still legal - the
// hazard check must not have banned arithmetic outright.
func TestSubtractionIsStillAllowed(t *testing.T) {
	if err := Validate(`source["my-source"].count - 1`); err != nil {
		t.Fatalf("subtracting a literal must remain legal: %v", err)
	}
	kind, err := TypeOf(`source["my-source"].count - 1`, demoSchemas)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kind != cue.IntKind {
		t.Fatalf("expected int, got %s", kind)
	}
}

// Membership of the readable context set comes from the cache-key rules, so the
// two cannot drift into disagreeing about what a consumer may read. Only the
// types are declared locally.
func TestContextTypesMatchTheKeyRules(t *testing.T) {
	rules, err := cachekey.LoadRules()
	if err != nil {
		t.Fatalf("loading rules: %v", err)
	}

	fromRules := map[string]bool{}
	for _, f := range rules.Fields() {
		fromRules[f] = true
	}
	for field := range contextTypes {
		if !fromRules[field] {
			t.Errorf("contextTypes declares %q, which the cache-key rules do not permit", field)
		}
	}
	for field := range fromRules {
		if _, ok := contextTypes[field]; !ok {
			t.Errorf("the cache-key rules permit %q but contextTypes gives it no type", field)
		}
	}
}

func TestContextTypeOf(t *testing.T) {
	cases := []struct {
		name    string
		expr    string
		want    cue.Kind
		wantErr string
	}{
		{name: "a plain field", expr: `context.cluster`, want: cue.StringKind},
		{name: "an indexed label", expr: `context.appLabels["cluster-name"]`, want: cue.StringKind},
		{name: "a label with a dotted key", expr: `context.appLabels["example.org/team"]`, want: cue.StringKind},
		{name: "combined with a literal", expr: `context.appLabels["cluster-name"] + "-suffix"`, want: cue.StringKind},
		{name: "combined with a source", expr: `context.cluster + "/" + source["other"].name`, want: cue.StringKind},
		{name: "a nested struct field", expr: `context.clusterVersion.minor`, want: cue.StringKind},
		{name: "a numeric field", expr: `context.appRevisionNum * 2`, want: cue.IntKind},
		{
			// The same curated set the cache-key rules define, so a field that is
			// not readable by a source is not readable here either.
			name:    "an unsupported field is rejected",
			expr:    `context.componentType`,
			wantErr: "not a supported value",
		},
		{
			name:    "context.output stays unreachable",
			expr:    `context.output.metadata.name`,
			wantErr: "not a supported value",
		},
		{
			name:    "an indexed field read without a key",
			expr:    `context.appLabels`,
			wantErr: "must be read with a key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TypeOf(tc.expr, demoSchemas)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected an error mentioning %q, got %v (kind %s)", tc.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected kind %s, got %s", tc.want, got)
			}
		})
	}
}

func TestContextEval(t *testing.T) {
	ctx := map[string]interface{}{
		"cluster":   "prod",
		"namespace": "team-a",
		"appLabels": map[string]interface{}{"cluster-name": "eu-west-1"},
	}
	resolved := map[string]map[string]interface{}{
		"other": {"name": "checkout"},
	}

	for _, tc := range []struct {
		name string
		raw  string
		want interface{}
	}{
		{name: "the motivating case", raw: `$(context.appLabels["cluster-name"])`, want: "eu-west-1"},
		{name: "combined with a literal", raw: `$(context.appLabels["cluster-name"] + "-cluster")`, want: "eu-west-1-cluster"},
		{name: "combined with a source", raw: `$(context.cluster + "/" + source["other"].name)`, want: "prod/checkout"},
		{name: "embedded in text", raw: `https://$(context.namespace).svc`, want: "https://team-a.svc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Eval(tc.raw, resolved, ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %#v, got %#v", tc.want, got)
			}
		})
	}
}

// The main ergonomic gap, recorded as a test so it is not forgotten.
//
// Admission cannot know whether a label exists, so an absent one can only fail
// at render. Defaulting it needs a syntax this spike does not have: conditionals
// are barred by the grammar gate, and CUE's `|` is a disjunction rather than a
// fallback - `x | "default"` yields "default" even when x is present.
func TestAbsentLabelFailsAtRenderNotAdmission(t *testing.T) {
	expr := `context.appLabels["missing"]`

	if _, err := TypeOf(expr, demoSchemas); err != nil {
		t.Fatalf("admission cannot know the label is absent and must accept: %v", err)
	}

	_, err := Eval("$("+expr+")", nil, map[string]interface{}{
		"appLabels": map[string]interface{}{"present": "yes"},
	})
	if err == nil {
		t.Fatal("an absent label must fail rather than resolve to an empty string")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("the error should name the label that is absent; got: %v", err)
	}
}
