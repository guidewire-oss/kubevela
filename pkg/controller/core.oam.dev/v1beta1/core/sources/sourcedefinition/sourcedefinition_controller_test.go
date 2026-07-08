package sourcedefinition

import (
	"strings"
	"testing"
)

func TestExtractSchemaExpr(t *testing.T) {
	template := `
schema: {
  image: string
  nested?: {
    tag: string
  }
}
output: {
  image: parameter.image
}
parameter: {
  image: string
}
`
	schema, err := extractSchemaExpr(template)
	if err != nil {
		t.Fatalf("extractSchemaExpr returned error: %v", err)
	}
	if schema == "" {
		t.Fatalf("extractSchemaExpr returned empty schema")
	}
}

func TestBuildSchemaTemplateNameStable(t *testing.T) {
	a := buildSchemaTemplateName("img-source", "abc123456789")
	b := buildSchemaTemplateName("img-source", "abc123456789")
	c := buildSchemaTemplateName("img-source", "def456456789")
	if a != b {
		t.Fatalf("expected deterministic name, got %s vs %s", a, b)
	}
	if a == c {
		t.Fatalf("expected different hash to produce different name")
	}
	if !strings.HasPrefix(a, "source-img-source-abc12345") {
		t.Fatalf("expected source name + short schema hash in template name, got %s", a)
	}
}

func TestBuildSchemaTemplateNameTruncatesAndSanitizes(t *testing.T) {
	name := "My_Source.Definition.With$Long@@Name-That-Keeps-Going-Longer-And-Longer"
	got := buildSchemaTemplateName(name, "0011223344556677")
	if len(got) > 63 {
		t.Fatalf("template name exceeds max length: %d", len(got))
	}
	if !strings.HasPrefix(got, "source-") {
		t.Fatalf("template name prefix invalid: %s", got)
	}
	if !strings.HasSuffix(got, "-00112233") {
		t.Fatalf("template hash suffix invalid: %s", got)
	}
}
