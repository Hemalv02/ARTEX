package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file locks down the **capability boundary** of the generic Webhook template.
//
// This is the only place in the whole package where "a user-provided string is evaluated as code", so it must be clear what it can and
// cannot do, with tests pinning those properties down — otherwise someone later casually adds a method to the template context
// or a readFile to the FuncMap, and the capability surface silently expands while the diff looks like
// just a harmless little function.

// TestTemplateContextHasNoMethods is the most important one.
//
// text/template calls exported methods ({{.Foo}} can fetch a field or call a method). So the moment the template context
// can reach **any** type with an exported method, those methods are exposed to the template author.
// This feature's context is deliberately all pure data (exported fields only, zero methods).
//
// If this fails: someone added a method to webhookTemplateData / webhookItem.
// Before deciding to allow it, think through whether that method can be used by a template to read something you do not want exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposed %d methods (%s): text/template can call them, "+
				"which opens up the capability of those methods to the template author", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal locks down the set of functions exposed to templates.
//
// Every extra function in the FuncMap is one more capability. Currently there are only json / jsons, whose job is to serialize a value
// into a JSON fragment — they cannot read files, send requests, or execute commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the template function set changed: got %v, expected %v. Before adding a function, confirm it does not widen the capability surface "+
			"(cannot read/write files, make network requests, or execute commands)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds access in a template:
// accessing something that does not exist must fail rather than echo anything, and the failure message must not carry out internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a nonexistent field should error")
	}
	// The error must not contain the real content of the template context (finding title/summary).
	for _, leak := range []string{"SQL注入", "参数 id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: a wrong template is a configuration error, and retrying will not self-heal.
// If judged retryable, one bad template makes every delivery waste three rounds of backoff.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("a template syntax error should be caught at save time")
	}
	// Even if validation is bypassed and delivery is attempted directly, it must be judged permanent rather than retried repeatedly.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a bad template should be judged permanent, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the constraint that "a template's render result must be valid JSON".
// It incidentally blocks uses like "generate plain text with a template to trigger another protocol".
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// A valid template passes.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("a valid template should pass validation: %v", err)
	}
	// Rendering non-JSON must be rejected (not sent as-is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("rendering non-JSON should be judged permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("the error message should state it is a JSON problem, got %v", err)
	}
}
