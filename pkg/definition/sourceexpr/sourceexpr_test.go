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
	"cuelang.org/go/cue/cuecontext"

	velaprocess "github.com/oam-dev/kubevela/pkg/cue/process"
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

// An expression sees what the definition it feeds sees, at the moment that
// definition is rendered. This builds a real component render context and
// requires every field in it to be classified - readable with a type, or
// explicitly not readable with a reason.
//
// The point is drift: a field added to the render context upstream must force a
// decision here rather than silently becoming unavailable, which would be
// indistinguishable from a typo in an author's expression.
func TestContextTypesMatchTheRenderContext(t *testing.T) {
	ctx := velaprocess.NewContext(velaprocess.ContextData{
		AppName: "checkout", CompName: "web", Namespace: "team-a",
		AppRevisionName: "checkout-v3", WorkflowName: "deploy", PublishVersion: "v1",
		Cluster:        "prod",
		AppLabels:      map[string]string{"team": "payments"},
		AppAnnotations: map[string]string{"note": "x"},
	})
	base, err := ctx.BaseContextFile()
	if err != nil {
		t.Fatalf("building the render context: %v", err)
	}
	v := cuecontext.New().CompileString(base)
	if v.Err() != nil {
		t.Fatalf("compiling the render context: %v", v.Err())
	}
	iter, err := v.LookupPath(cue.ParsePath("context")).Fields(cue.All())
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for iter.Next() {
		field := iter.Selector().Unquoted()
		seen[field] = true
		_, readable := ComponentContext.types[field]
		_, excluded := ComponentContext.notReadable[field]
		switch {
		case readable && excluded:
			t.Errorf("%q is both readable and excluded; it must be one or the other", field)
		case !readable && !excluded:
			t.Errorf("the render context carries %q (%s) but this package neither types it nor "+
				"explains why it is unavailable - classify it in ComponentContext",
				field, iter.Value().IncompleteKind())
		}
	}

	// A type declared for a field the render context does not carry would type
	// cleanly at admission and fail at render.
	for field := range ComponentContext.types {
		if !seen[field] {
			t.Errorf("ComponentContext declares %q, which the render context does not carry", field)
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
		// minor is an int64 in parseClusterVersion, major a string. The sentinel
		// has to match, or admission promises a type render will not produce.
		{name: "a nested struct field", expr: `context.clusterVersion.minor`, want: cue.IntKind},
		{name: "a nested string field", expr: `context.clusterVersion.major`, want: cue.StringKind},
		{name: "a numeric field", expr: `context.appRevisionNum * 2`, want: cue.IntKind},
		{
			// The same curated set the cache-key rules define, so a field that is
			// not readable by a source is not readable here either.
			name:    "an unsupported field is rejected",
			expr:    `context.componentType`,
			wantErr: "is not readable in component properties",
		},
		{
			name:    "context.output stays unreachable",
			expr:    `context.output.metadata.name`,
			wantErr: "is not readable in component properties",
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

// Getting an int into an int parameter is the common case, and there is one
// trap: CUE's `/` is float division, so `replicas / 2` types as float and would
// be refused by an int parameter. CUE's integer operators are infix - div, mod,
// quo, rem - and they type as int.
func TestIntegerValuedExpressions(t *testing.T) {
	schemas := map[string]string{"scale": `{replicas: int}`}
	resolved := map[string]map[string]interface{}{"scale": {"replicas": 7}}

	cases := []struct {
		expr      string
		wantKind  cue.Kind
		wantValue interface{}
	}{
		{`source["scale"].replicas`, cue.IntKind, int64(7)},
		{`source["scale"].replicas * 2`, cue.IntKind, int64(14)},
		{`source["scale"].replicas + 1`, cue.IntKind, int64(8)},
		{`source["scale"].replicas div 2`, cue.IntKind, int64(3)},
		{`source["scale"].replicas mod 2`, cue.IntKind, int64(1)},
		// The trap: ordinary division is float division.
		{`source["scale"].replicas / 2`, cue.FloatKind, 3.5},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			kind, err := TypeOf(tc.expr, schemas)
			if err != nil {
				t.Fatalf("typing: %v", err)
			}
			if kind != tc.wantKind {
				t.Fatalf("expected kind %s, got %s", tc.wantKind, kind)
			}
			got, err := Eval("$("+tc.expr+")", resolved, nil)
			if err != nil {
				t.Fatalf("evaluating: %v", err)
			}
			if got != tc.wantValue {
				t.Fatalf("expected %#v, got %#v", tc.wantValue, got)
			}
		})
	}
}

// An int-valued expression embedded in surrounding text is a string, because
// that is what concatenation means. Only a whole value keeps its type - which is
// the distinction that lets an int parameter receive an int at all.
func TestEmbeddedIntBecomesString(t *testing.T) {
	resolved := map[string]map[string]interface{}{"scale": {"replicas": 7}}

	whole, err := Eval(`$(source["scale"].replicas)`, resolved, nil)
	if err != nil {
		t.Fatal(err)
	}
	if whole != int64(7) {
		t.Fatalf("a whole value must keep its type, got %#v (%T)", whole, whole)
	}

	embedded, err := Eval(`replicas-$(source["scale"].replicas)`, resolved, nil)
	if err != nil {
		t.Fatal(err)
	}
	if embedded != "replicas-7" {
		t.Fatalf("an embedded value must render as text, got %#v", embedded)
	}
}

// Surrounding whitespace must not change the substituted type.
//
// A single trailing space is invisible in YAML and trivial to leave behind when
// editing. If it flipped an int into a string, the failure would surface as a
// type mismatch against the parameter - nowhere near the space that caused it.
func TestWhitespaceCannotChangeTheType(t *testing.T) {
	resolved := map[string]map[string]interface{}{"s": {"count": 3}}

	for _, raw := range []string{
		`$(source["s"].count)`,
		`$(source["s"].count) `,
		` $(source["s"].count)`,
		"\t$(source[\"s\"].count)\n",
	} {
		got, err := Eval(raw, resolved, nil)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got != int64(3) {
			t.Errorf("%q substituted %#v (%T); whitespace must not make it a string", raw, got, got)
		}
	}

	// A visible character still makes it a string - that is a deliberate act.
	got, err := Eval(`$(source["s"].count)x`, resolved, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "3x" {
		t.Errorf("expected %q, got %#v", "3x", got)
	}
}

// Values with no expression are returned exactly as written, whitespace and all.
// Only a value containing the delimiter is touched.
func TestPlainStringsAreUntouched(t *testing.T) {
	for _, raw := range []string{
		"nginx:1.25.0",
		"  leading and trailing  ",
		"a string with (parens) and a $ sign",
		"100%",
		"",
	} {
		got, err := Eval(raw, nil, nil)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		if got != raw {
			t.Errorf("expected %q to be returned unchanged, got %#v", raw, got)
		}
	}
}

// The dotted form is the normal way to name a binding, and the one nearly every
// real binding name supports - clusterInfo, tenant, img, scaleData are all legal
// CUE identifiers. Brackets are the exception, needed only when the name is not.
func TestDottedBindingNames(t *testing.T) {
	schemas := map[string]string{
		"checkout":    `{name: string, tag: string}`,
		"clusterInfo": `{region: string}`,
	}
	resolved := map[string]map[string]interface{}{
		"checkout":    {"name": "web", "tag": "1.0"},
		"clusterInfo": {"region": "eu-west"},
	}

	for _, tc := range []struct {
		expr string
		want interface{}
	}{
		{`source.checkout.name`, "web"},
		{`source.clusterInfo.region`, "eu-west"},
		{`source.checkout.name + ":" + source.checkout.tag`, "web:1.0"},
		// Both spellings address the same binding.
		{`source["checkout"].name`, "web"},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			if _, err := TypeOf(tc.expr, schemas); err != nil {
				t.Fatalf("typing: %v", err)
			}
			got, err := Eval("$("+tc.expr+")", resolved, nil)
			if err != nil {
				t.Fatalf("evaluating: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %#v, got %#v", tc.want, got)
			}
		})
	}

	// And References reports the same path either way, so downstream consumers -
	// schema validation, dependency ordering, sensitive taint - do not have to
	// care which spelling the author used.
	dotted, err := References(`source.checkout.name`)
	if err != nil {
		t.Fatal(err)
	}
	bracketed, err := References(`source["checkout"].name`)
	if err != nil {
		t.Fatal(err)
	}
	if len(dotted) != 1 || len(bracketed) != 1 || dotted[0].String() != bracketed[0].String() {
		t.Fatalf("the two spellings must resolve to one reference; got %v and %v", dotted, bracketed)
	}
}

// Parity with fromSource's `default:`. Without this, expressions could not
// replace fromSource - they would be strictly less capable, since a binding
// whose value may be absent has no way to carry a fallback.
//
// The marker sits on the value, not the fallback. The two obvious alternatives
// are both silently wrong: `x | *"d"` yields the default even when x is present,
// and `x | "d"` is an ambiguous disjunction rather than a fallback.
func TestDefaultForAnAbsentValue(t *testing.T) {
	schemas := map[string]string{"img": `{image: string}`}

	// Types cleanly: at admission the value is present, so the result is the
	// value's type - which the default must match.
	kind, err := TypeOf(`*source.img.image | "nginx:latest"`, schemas)
	if err != nil {
		t.Fatalf("a defaulted read must type: %v", err)
	}
	if kind != cue.StringKind {
		t.Fatalf("expected string, got %s", kind)
	}

	present := map[string]map[string]interface{}{"img": {"image": "nginx:1.25"}}
	got, err := Eval(`$(*source.img.image | "nginx:latest")`, present, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "nginx:1.25" {
		t.Errorf("a present value must win over the default, got %#v", got)
	}

	absent := map[string]map[string]interface{}{"img": {"other": "x"}}
	got, err = Eval(`$(*source.img.image | "nginx:latest")`, absent, nil)
	if err != nil {
		t.Fatalf("an absent value must fall back rather than fail: %v", err)
	}
	if got != "nginx:latest" {
		t.Errorf("expected the fallback, got %#v", got)
	}
}

// The same mechanism closes the absent-label gap.
func TestDefaultForAnAbsentContextLabel(t *testing.T) {
	expr := `*context.appLabels["region"] | "unknown"`

	if _, err := TypeOf(expr, demoSchemas); err != nil {
		t.Fatalf("typing: %v", err)
	}
	got, err := Eval("$("+expr+")", nil, map[string]interface{}{
		"appLabels": map[string]interface{}{"team": "payments"},
	})
	if err != nil {
		t.Fatalf("an absent label with a default must not fail: %v", err)
	}
	if got != "unknown" {
		t.Errorf("expected the fallback, got %#v", got)
	}
}

// A default of the wrong type is caught at admission. At render only one branch
// is taken, so the mismatch could otherwise go unnoticed until the day the value
// happens to be absent.
func TestDefaultTypeMustMatch(t *testing.T) {
	schemas := map[string]string{"s": `{count: int}`}

	_, err := TypeOf(`*source.s.count | "none"`, schemas)
	if err == nil {
		t.Fatal("a string default for an int value must be rejected")
	}
	if !strings.Contains(err.Error(), "same type") {
		t.Errorf("the error should explain the rule; got: %v", err)
	}

	if _, err := TypeOf(`*source.s.count | 0`, schemas); err != nil {
		t.Fatalf("a matching default must be accepted: %v", err)
	}
}

// Only the defaulting shape is allowed. A general disjunction is still refused,
// because its result type would depend on which branch a value selected.
func TestOnlyTheDefaultShapeOfDisjunctionIsAllowed(t *testing.T) {
	for _, expr := range []string{
		`source.img.image | "nginx"`,         // no marker - ambiguous, not a fallback
		`source.img.image | *"nginx"`,        // marker on the wrong side
		`*source.img.image | source.other.x`, // non-literal fallback
		`*"literal" | "other"`,               // marker not on a read
	} {
		if err := Validate(expr); err == nil {
			t.Errorf("expected %q to be rejected", expr)
		}
	}

	if err := Validate(`*source.img.image | "nginx"`); err != nil {
		t.Errorf("the defaulting shape must be accepted: %v", err)
	}
}

// ValueType is what admission compares against the parameter. It has to answer
// for a whole property value, not just one expression - a value may be plain
// text, one expression, or several interleaved with text.
func TestValueType(t *testing.T) {
	schemas := map[string]string{
		"a": `{x: string, n: int, obj: {k: string}, items: [...string]}`,
		"b": `{y: string}`,
	}

	cases := []struct {
		name    string
		raw     string
		want    cue.Kind
		wantErr string
	}{
		{name: "a literal is a string", raw: "nginx:1.25", want: cue.StringKind},
		{name: "a lone expression keeps its type", raw: `$(source.a.n)`, want: cue.IntKind},
		{name: "a lone string expression", raw: `$(source.a.x)`, want: cue.StringKind},
		{
			// The case that motivated this: two expressions concatenate, so the
			// value is a string no matter what the parts produce.
			name: "two expressions concatenate to a string",
			raw:  `$(source.a.x) $(source.b.y)`,
			want: cue.StringKind,
		},
		{
			name: "an int among text is still a string",
			raw:  `port-$(source.a.n)`,
			want: cue.StringKind,
		},
		{
			// Knowable from the shape alone: an int parameter can be told at
			// admission that this will never satisfy it.
			name: "two int expressions still make a string",
			raw:  `$(source.a.n)$(source.a.n)`,
			want: cue.StringKind,
		},
		{
			// Previously only caught at render, after admission had passed.
			name:    "a struct cannot be combined with text",
			raw:     `$(source.a.obj) suffix`,
			wantErr: "cannot be combined with text",
		},
		{
			name:    "a list cannot be combined with text",
			raw:     `items: $(source.a.items)!`,
			wantErr: "cannot be combined with text",
		},
		{
			// A struct on its own is fine - it is not being concatenated.
			name: "a struct alone keeps its type",
			raw:  `$(source.a.obj)`,
			want: cue.StructKind,
		},
		{
			name:    "an error inside any fragment is reported",
			raw:     `$(source.a.x) $(source.b.undeclared)`,
			wantErr: "not declared in the source's schema",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValueType(tc.raw, schemas)
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
				t.Fatalf("expected %s, got %s", tc.want, got)
			}
		})
	}
}

// Each expression carries its own default; there is no outer one. A default on
// the whole value could not know which part was absent, and would mask the rest.
func TestEachExpressionDefaultsIndependently(t *testing.T) {
	srcs := map[string]map[string]interface{}{
		"a": {"x": "alpha"},
		"b": {},
	}

	// One absent value with no default fails the whole value, even though the
	// other fragment resolved.
	if _, err := Eval(`$(source.a.x) $(source.b.y)`, srcs, nil); err == nil {
		t.Fatal("an absent value without a default must fail the whole property")
	}

	got, err := Eval(`$(source.a.x) $(*source.b.y | "fallback")`, srcs, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "alpha fallback" {
		t.Fatalf("expected %q, got %#v", "alpha fallback", got)
	}
}

// ValueType and Eval must agree, or admission is checking something the render
// does not produce.
func TestValueTypeAgreesWithEval(t *testing.T) {
	schemas := map[string]string{"a": `{x: string, n: int, ok: bool}`}
	srcs := map[string]map[string]interface{}{"a": {"x": "alpha", "n": 3, "ok": true}}

	for _, raw := range []string{
		"literal",
		`$(source.a.x)`,
		`$(source.a.n)`,
		`$(source.a.ok)`,
		`$(source.a.x) $(source.a.n)`,
		`prefix-$(source.a.n)`,
	} {
		kind, err := ValueType(raw, schemas)
		if err != nil {
			t.Fatalf("%s: typing: %v", raw, err)
		}
		value, err := Eval(raw, srcs, nil)
		if err != nil {
			t.Fatalf("%s: evaluating: %v", raw, err)
		}

		var actual cue.Kind
		switch value.(type) {
		case string:
			actual = cue.StringKind
		case bool:
			actual = cue.BoolKind
		case int64, int:
			actual = cue.IntKind
		case float64:
			actual = cue.FloatKind
		default:
			t.Fatalf("%s: unexpected result type %T", raw, value)
		}
		if actual != kind {
			t.Errorf("%s: admission promised %s, render produced %s (%#v)", raw, kind, actual, value)
		}
	}
}

// A default is only needed where a value could actually be missing. A source's
// schema is a contract the resolver enforces - output is validated against it
// before anything is cached - so a required field is guaranteed present and
// demanding a fallback for it would be noise.
//
// This mirrors fromSource, where a default is mandatory exactly when an optional
// source field feeds a required parameter.
func TestUndefendedReads(t *testing.T) {
	schemas := map[string]string{
		"info": `{
			region:  string
			vpcId?:  string
			network?: {subnet: string}
			nested:  {zone: string, backup?: string}
		}`,
	}

	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "a required field needs no default",
			raw:  `$(source.info.region)`,
		},
		{
			name: "a required nested field needs no default",
			raw:  `$(source.info.nested.zone)`,
		},
		{
			name: "an optional field is undefended",
			raw:  `$(source.info.vpcId)`,
			want: []string{"source.info.vpcId"},
		},
		{
			name: "an optional field with a default is fine",
			raw:  `$(*source.info.vpcId | "none")`,
		},
		{
			// network? is optional, so subnet is absent whenever network is,
			// however required it looks inside.
			name: "an optional ancestor makes a required field absent",
			raw:  `$(source.info.network.subnet)`,
			want: []string{"source.info.network.subnet"},
		},
		{
			name: "an optional leaf under a required parent",
			raw:  `$(source.info.nested.backup)`,
			want: []string{"source.info.nested.backup"},
		},
		{
			// A label lookup has no schema at all - any key may be missing.
			name: "a context label is always undefended",
			raw:  `$(context.appLabels["region"])`,
			want: []string{"context.appLabels.region"},
		},
		{
			name: "a defaulted context label is fine",
			raw:  `$(*context.appLabels["region"] | "unknown")`,
		},
		{
			// A plain context field is always supplied, even when empty.
			name: "a plain context field needs no default",
			raw:  `$(context.cluster)`,
		},
		{
			name: "several fragments are each checked",
			raw:  `$(source.info.region)/$(source.info.vpcId)`,
			want: []string{"source.info.vpcId"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := UndefendedReads(tc.raw, schemas)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
			for i := range tc.want {
				if got[i].String() != tc.want[i] {
					t.Fatalf("expected %v, got %v", tc.want, got)
				}
			}
		})
	}
}

// A path read both with and without a default is still undefended: the
// unprotected read is the one that fails.
func TestUndefendedWhenReadBothWays(t *testing.T) {
	schemas := map[string]string{"info": `{vpcId?: string}`}

	got, err := UndefendedReads(`$(*source.info.vpcId | "a")$(source.info.vpcId)`, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the undefended read must still be reported, got %v", got)
	}
}

// The scopes an expression is evaluated against are assembled by concatenating
// CUE source text, and binding names and label keys come from the Application
// spec - so they are attacker-controlled if the author is hostile. A name that
// escaped its quotes would inject arbitrary CUE into the scope.
//
// Quoting is handled by %q and json.Marshal. This pins it, because the failure
// would be silent: the injected fields would simply be there.
func TestScopeQuotingResistsInjection(t *testing.T) {
	const breakout = `a": {pwned: "yes"}, "b`

	schemas := map[string]string{
		breakout: `{x: string}`,
		"normal": `{x: string}`,
	}

	// The injected field must not exist in the scope.
	if _, err := TypeOf(`source.pwned`, schemas); err == nil {
		t.Fatal("a hostile binding name injected a field into the sentinel scope")
	}
	// The legitimate one still works, so the escaping did not simply break
	// everything.
	if _, err := TypeOf(`source.normal.x`, schemas); err != nil {
		t.Fatalf("escaping must not break an ordinary name: %v", err)
	}

	// The same at render, through the values scope and the context scope.
	values := map[string]map[string]interface{}{
		breakout: {"x": "v"},
		"normal": {"x": "v"},
	}
	ctx := map[string]interface{}{
		"appLabels": map[string]interface{}{`k": 1, "j`: "v"},
	}
	got, err := Eval(`$(source.normal.x)`, values, ctx)
	if err != nil {
		t.Fatalf("hostile keys must not break evaluation: %v", err)
	}
	if got != "v" {
		t.Fatalf("expected %q, got %#v", "v", got)
	}

	// A hostile label key is reachable as data, not as structure.
	label, err := Eval(`$(context.appLabels["k\": 1, \"j"])`, nil, ctx)
	if err != nil {
		t.Fatalf("a label key containing quotes must still be readable: %v", err)
	}
	if label != "v" {
		t.Fatalf("expected %q, got %#v", "v", label)
	}
}

// The expression is still concatenated into `out: <expr>` before compiling, so a
// value carrying a newline and another field would add that field to the compiled
// file. Validate rejects it because parser.ParseExpr demands a single expression,
// and evalIn re-checks so the guarantee does not depend on call order.
func TestExpressionCannotEscapeTheWrapper(t *testing.T) {
	schemas := map[string]string{"s": `{x: string}`}
	values := map[string]map[string]interface{}{"s": {"x": "v"}}

	for _, expr := range []string{
		"source.s.x\nevil: 1",
		"source.s.x}\nevil: {a: 1",
		`source.s.x, evil: 1`,
		"source.s.x // comment\nevil: 1",
	} {
		if _, err := TypeOf(expr, schemas); err == nil {
			t.Errorf("TypeOf accepted an expression that adds a field: %q", expr)
		}
		if _, err := evalFragment(expr, values, nil, ComponentContext, []string{SourceIdent, ContextIdent}); err == nil {
			t.Errorf("evalFragment accepted an expression that adds a field: %q", expr)
		}
	}
}

// The context equivalent of TestTypeOfAgreesWithEval, and the test that would
// have caught clusterVersion.minor being declared a string when
// parseClusterVersion builds it with strconv.ParseInt.
//
// The context values below mirror what process.NewContext actually pushes; a
// sentinel that disagrees with them makes admission promise a type the render
// will not produce.
func TestContextTypeOfAgreesWithEval(t *testing.T) {
	ctx := map[string]interface{}{
		"name":           "web",
		"cluster":        "prod",
		"namespace":      "team-a",
		"appName":        "checkout",
		"appRevision":    "checkout-v3",
		"appRevisionNum": int64(3),
		"publishVersion": "v1",
		"workflowName":   "deploy",
		"appLabels":      map[string]interface{}{"team": "payments"},
		"appAnnotations": map[string]interface{}{"note": "x"},
		"clusterVersion": map[string]interface{}{
			"major": "1", "minor": int64(31), "gitVersion": "v1.31.0", "platform": "linux/amd64",
		},
	}

	for _, expr := range []string{
		`context.name`,
		`context.cluster`,
		`context.namespace`,
		`context.appName`,
		`context.appRevision`,
		`context.appRevisionNum`,
		`context.publishVersion`,
		`context.workflowName`,
		`context.appLabels["team"]`,
		`context.appAnnotations["note"]`,
		`context.clusterVersion.major`,
		`context.clusterVersion.minor`,
		`context.clusterVersion.gitVersion`,
		`context.clusterVersion.platform`,
	} {
		kind, err := TypeOf(expr, nil)
		if err != nil {
			t.Errorf("%s: typing: %v", expr, err)
			continue
		}
		value, err := Eval("$("+expr+")", nil, ctx)
		if err != nil {
			t.Errorf("%s: evaluating: %v", expr, err)
			continue
		}

		var actual cue.Kind
		switch value.(type) {
		case string:
			actual = cue.StringKind
		case bool:
			actual = cue.BoolKind
		case int, int64:
			actual = cue.IntKind
		case float64:
			actual = cue.FloatKind
		default:
			t.Errorf("%s: unexpected result type %T", expr, value)
			continue
		}
		if actual != kind {
			t.Errorf("%s: admission promised %s, render produced %s (%#v)", expr, kind, actual, value)
		}
	}
}

// A surface can support one root and not the other, which is what lets a policy
// carry expressions at all.
//
// An Application-scoped policy renders before the appfile exists, so there is no
// parsed spec.sources[] to resolve against - but its context is built by hand for
// that render and is fully available. Permitting context and withholding source
// is the difference between such a surface having the feature and being excluded
// from it.
func TestValidateRootsRestrictsPerSurface(t *testing.T) {
	contextOnly := []string{ContextIdent}

	for _, expr := range []string{
		`context.appName`,
		`context.appLabels["team"]`,
		`context.appName + "-suffix"`,
		`*context.appLabels["team"] | "none"`,
	} {
		if err := ValidateRoots(expr, contextOnly...); err != nil {
			t.Errorf("expected %q to be allowed with context only, got: %v", expr, err)
		}
	}

	// Reading source on such a surface is rejected with a reason, rather than
	// substituted with nothing or left inert as a literal directive.
	for _, expr := range []string{
		`source.img.image`,
		`context.appName + source.img.image`,
		`source["img"].image`,
	} {
		err := ValidateRoots(expr, contextOnly...)
		if err == nil {
			t.Errorf("expected %q to be rejected where sources cannot resolve", expr)
			continue
		}
		if !strings.Contains(err.Error(), "cannot be read here") {
			t.Errorf("the error should say the surface does not permit it; got: %v", err)
		}
	}

	// The default remains both roots, so nothing else changes.
	if err := Validate(`context.appName + source.img.image`); err != nil {
		t.Errorf("Validate must still permit both roots: %v", err)
	}
}

// Restricting roots must not weaken the sandbox: an unknown identifier is still
// rejected, and the message names what is permitted here rather than in general.
func TestValidateRootsKeepsTheSandbox(t *testing.T) {
	err := ValidateRoots(`parameter.image`, ContextIdent)
	if err == nil {
		t.Fatal("an unknown identifier must still be rejected")
	}
	if !strings.Contains(err.Error(), `"context"`) {
		t.Errorf("the message should name what this surface permits; got: %v", err)
	}
	if strings.Contains(err.Error(), `"source"`) {
		t.Errorf("the message should not offer a root this surface forbids; got: %v", err)
	}
}

// scopedPolicyContext mirrors renderPolicyCUETemplate's construction, so the
// schema is checked against what that render really carries.
func scopedPolicyContext(t *testing.T) string {
	t.Helper()
	pCtx := velaprocess.NewContext(velaprocess.ContextData{
		Namespace:       "team-a",
		AppName:         "checkout",
		CompName:        "checkout",
		AppRevisionName: "checkout-v3",
		AppLabels:       map[string]string{"team": "payments"},
		AppAnnotations:  map[string]string{"note": "x"},
	})
	pCtx.PushData(velaprocess.ContextAppComponents, []string{})
	pCtx.PushData(velaprocess.ContextAppWorkflow, nil)
	pCtx.PushData(velaprocess.ContextAppPolicies, []string{})
	pCtx.PushData(velaprocess.ContextPolicyName, "my-policy")
	pCtx.PushData(velaprocess.ContextPolicyType, "my-type")
	pCtx.PushData(velaprocess.ContextPolicyRevisionName, "rev-1")
	pCtx.PushData(velaprocess.ContextPolicyRevision, int64(1))
	pCtx.PushData(velaprocess.ContextPolicyRevisionHash, "abc")

	base, err := pCtx.BaseContextFile()
	if err != nil {
		t.Fatalf("building the scoped policy context: %v", err)
	}
	return base
}

// The same drift guard as the component context, against the surface that
// actually differs. Its point is that a field a surface never populates must not
// be declared readable: it would type cleanly at admission and hand the author an
// empty value at render.
func TestScopedPolicyContextMatchesTheRender(t *testing.T) {
	v := cuecontext.New().CompileString(scopedPolicyContext(t))
	if v.Err() != nil {
		t.Fatalf("compiling: %v", v.Err())
	}
	iter, err := v.LookupPath(cue.ParsePath("context")).Fields(cue.All())
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for iter.Next() {
		field := iter.Selector().Unquoted()
		seen[field] = true
		_, readable := ScopedPolicyContext.types[field]
		_, excluded := ScopedPolicyContext.notReadable[field]
		switch {
		case readable && excluded:
			t.Errorf("%q is both readable and excluded", field)
		case !readable && !excluded:
			t.Errorf("the scoped policy context carries %q (%s) but ScopedPolicyContext neither "+
				"types it nor explains why it is unavailable", field, iter.Value().IncompleteKind())
		}

		// A readable field must actually be populated. An always-empty one would
		// type at admission and be useless at render.
		if readable {
			if s, err := iter.Value().String(); err == nil && s == "" {
				t.Errorf("%q is declared readable but is empty in a real scoped policy render", field)
			}
		}
	}

	for field := range ScopedPolicyContext.types {
		if !seen[field] {
			t.Errorf("ScopedPolicyContext declares %q, which the scoped render does not carry", field)
		}
	}
}

// The subset is the point: fields the component context has, that a scoped
// policy never receives, must be refused there.
func TestScopedPolicyContextIsASubset(t *testing.T) {
	for _, expr := range []string{
		`context.appName`,
		`context.namespace`,
		`context.appLabels["team"]`,
		`context.policyName`,
	} {
		if _, err := TypeOfIn(expr, nil, ScopedPolicyContext, ContextIdent); err != nil {
			t.Errorf("expected %q to be readable in a scoped policy: %v", expr, err)
		}
	}

	for _, tc := range []struct{ expr, why string }{
		{`context.cluster`, "always empty"},
		{`context.workflowName`, "always empty"},
		{`context.publishVersion`, "always empty"},
		{`context.name`, "ambiguous across the two policy paths"},
	} {
		_, err := TypeOfIn(tc.expr, nil, ScopedPolicyContext, ContextIdent)
		if err == nil {
			t.Errorf("expected %q to be refused in a scoped policy (%s)", tc.expr, tc.why)
			continue
		}
		if !strings.Contains(err.Error(), "application-scoped policy") {
			t.Errorf("the error should name the surface; got: %v", err)
		}
	}

	// policyName is the scoped context's answer to context.name, and the
	// component context has no equivalent.
	if _, err := TypeOfIn(`context.policyName`, nil, ComponentContext, ContextIdent); err == nil {
		t.Error("policyName must not be readable in a component, where it does not exist")
	}

	// And sources stay unreadable there regardless of the context schema.
	if _, err := TypeOfIn(`source.img.image`, nil, ScopedPolicyContext, ContextIdent); err == nil {
		t.Error("a scoped policy cannot resolve sources and must refuse to read them")
	}
}

// Complex types: a struct or list expression keeps its type when it is the whole
// value, so it can feed a struct or list parameter. Only concatenation forces a
// string, and a struct has no text form to concatenate.
func TestComplexTypes(t *testing.T) {
	schemas := map[string]string{
		"s": `{obj: {team: string, tier: string}, items: [...string], name: string}`,
	}
	resolved := map[string]map[string]interface{}{
		"s": {
			"obj":   map[string]interface{}{"team": "payments", "tier": "gold"},
			"items": []interface{}{"alpha", "beta"},
			"name":  "web",
		},
	}

	for _, tc := range []struct {
		raw  string
		kind cue.Kind
	}{
		{`$(source.s.obj)`, cue.StructKind},
		{`$(source.s.items)`, cue.ListKind},
		{`$(source.s.obj.team)`, cue.StringKind},
		{`$(source.s.obj.team + "/x")`, cue.StringKind},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ValueType(tc.raw, schemas)
			if err != nil {
				t.Fatalf("typing: %v", err)
			}
			if got != tc.kind {
				t.Fatalf("expected %s, got %s", tc.kind, got)
			}

			value, err := Eval(tc.raw, resolved, nil)
			if err != nil {
				t.Fatalf("evaluating: %v", err)
			}
			// The rendered value must have the shape admission promised.
			switch tc.kind {
			case cue.StructKind:
				if _, ok := value.(map[string]interface{}); !ok {
					t.Fatalf("expected a struct, got %T", value)
				}
			case cue.ListKind:
				if _, ok := value.([]interface{}); !ok {
					t.Fatalf("expected a list, got %T", value)
				}
			case cue.StringKind:
				if _, ok := value.(string); !ok {
					t.Fatalf("expected a string, got %T", value)
				}
			}
		})
	}

	// A struct has no single text form, so concatenating one is refused at
	// admission rather than rendered as something the author did not ask for.
	for _, raw := range []string{
		`$(source.s.obj)-suffix`,
		`prefix-$(source.s.items)`,
		`$(source.s.obj)$(source.s.items)`,
	} {
		if _, err := ValueType(raw, schemas); err == nil {
			t.Errorf("expected %q to be refused: a struct or list cannot be combined with text", raw)
		}
	}
}

// An open map in a source's schema - labels: [string]: string - declares the map
// but never a key. Two things follow, and neither was handled at first:
//
//   - the sentinel has no concrete field at any key, so typing reported
//     "undefined field: team" for a read the schema plainly permits;
//   - the key may be missing at render for exactly the reason a context label
//     may, so it needs a default when it feeds a required parameter.
func TestOpenMapInSourceSchema(t *testing.T) {
	schemas := map[string]string{
		"s": `{
			labels: [string]: string
			counts: [string]: int
			meta:   {region: string}
			name:   string
		}`,
	}

	kind, err := TypeOf(`source.s.labels["team"]`, schemas)
	if err != nil {
		t.Fatalf("a key read out of an open map must type: %v", err)
	}
	if kind != cue.StringKind {
		t.Fatalf("expected string, got %s", kind)
	}

	// The map's value type is what decides the kind, not the key.
	if kind, err := TypeOf(`source.s.counts["shards"] * 2`, schemas); err != nil {
		t.Errorf("an int-valued map should support arithmetic: %v", err)
	} else if kind != cue.IntKind {
		t.Errorf("expected int, got %s", kind)
	}

	// Possibly absent, so undefended without a fallback.
	undefended, err := UndefendedReads(`$(source.s.labels["team"])`, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if len(undefended) != 1 || undefended[0].String() != "source.s.labels.team" {
		t.Fatalf("a key read out of an open map may be absent and must be reported; got %v", undefended)
	}

	// With a fallback it is defended, like any other possibly-absent read.
	undefended, err = UndefendedReads(`$(*source.s.labels["team"] | "none")`, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if len(undefended) != 0 {
		t.Fatalf("a defaulted read must not be reported; got %v", undefended)
	}

	// A concrete field beside the pattern is not affected.
	if undefended, err := UndefendedReads(`$(source.s.meta.region)`, schemas); err != nil || len(undefended) != 0 {
		t.Errorf("a declared field must not be treated as possibly absent; got %v (%v)", undefended, err)
	}

	// And evaluation agrees with the typing.
	value, err := Eval(`$(source.s.labels["team"])`, map[string]map[string]interface{}{
		"s": {"labels": map[string]interface{}{"team": "payments"}},
	}, nil)
	if err != nil {
		t.Fatalf("evaluating: %v", err)
	}
	if value != "payments" {
		t.Fatalf("expected %q, got %#v", "payments", value)
	}
}

// Resolved source values arrive as JSON, so a schema's int is a Go float64 by
// the time it reaches the scope. Encoding that directly makes it a CUE float,
// and `count div 2` then fails with "invalid operands ... (type float and int)"
// - while admission, typing against an int sentinel, said it was fine.
//
// Found by a demo rather than by a test, which is why it is pinned here.
func TestIntegersSurviveTheJSONRoundTrip(t *testing.T) {
	schemas := map[string]string{"s": `{count: int, ratio: float}`}

	// Exactly what the resolver hands over: JSON-decoded, so every number is a
	// float64 regardless of what the schema declares.
	resolved := map[string]map[string]interface{}{
		"s": {"count": float64(6), "ratio": float64(1.5)},
	}

	for _, tc := range []struct {
		expr string
		want interface{}
	}{
		{`source.s.count div 2`, int64(3)},
		{`source.s.count * 2`, int64(12)},
		{`source.s.count mod 4`, int64(2)},
		{`source.s.count`, int64(6)},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			// Admission types it against an int sentinel...
			kind, err := TypeOf(tc.expr, schemas)
			if err != nil {
				t.Fatalf("typing: %v", err)
			}
			if kind != cue.IntKind {
				t.Fatalf("expected int at admission, got %s", kind)
			}
			// ...so render has to produce an int too.
			got, err := Eval("$("+tc.expr+")", resolved, nil)
			if err != nil {
				t.Fatalf("evaluating: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %#v (%T), got %#v (%T)", tc.want, tc.want, got, got)
			}
		})
	}

	// A genuine float stays a float.
	if got, err := Eval(`$(source.s.ratio)`, resolved, nil); err != nil {
		t.Fatalf("evaluating: %v", err)
	} else if got != 1.5 {
		t.Fatalf("expected 1.5, got %#v", got)
	}
}

// A schema may declare a field without declaring its shape - `content: _` is how
// a generic source says "whatever this file happens to be". Nothing can be typed
// through that, so the answer is "unknown" rather than a guess, and admission
// declines to judge rather than rejecting something valid.
func TestOpenFieldTypesAsUnknown(t *testing.T) {
	schemas := map[string]string{
		"file":  `{content: _}`,
		"typed": `{name: string}`,
	}

	// Reading into the open field, at any depth.
	for _, expr := range []string{
		`source.file.content`,
		`source.file.content.name`,
		`source.file.content.spec.replicas`,
	} {
		kind, err := TypeOf(expr, schemas)
		if err != nil {
			t.Errorf("%s: a read into an open field must not error, got: %v", expr, err)
			continue
		}
		if kind != cue.BottomKind {
			t.Errorf("%s: expected the unknown kind, got %s", expr, kind)
		}
	}

	// Unknown must not block a substitution, whatever the target expects.
	if !kindsCompatibleForTest(cue.BottomKind, cue.IntKind) {
		t.Error("unknown must be compatible with any target, or a generic source could feed nothing")
	}

	// Embedding one in text is allowed too - whether it has a text form is
	// unknowable here, and render will say.
	if _, err := ValueType(`prefix-$(source.file.content.name)`, schemas); err != nil {
		t.Errorf("an open read embedded in text must not be rejected at admission: %v", err)
	}

	// A declared field beside an open one still types normally, so the escape
	// hatch does not leak into the rest of the schema.
	if kind, err := TypeOf(`source.typed.name`, schemas); err != nil || kind != cue.StringKind {
		t.Errorf("a declared field must still type: %s (%v)", kind, err)
	}
}

// kindsCompatibleForTest mirrors the webhook's rule, which lives in another
// package: unknown on either side does not block.
func kindsCompatibleForTest(src, dst cue.Kind) bool {
	return src == cue.BottomKind || dst == cue.BottomKind || src&dst != 0
}
