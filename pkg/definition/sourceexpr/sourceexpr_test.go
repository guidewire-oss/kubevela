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
