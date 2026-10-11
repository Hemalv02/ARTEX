package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file locks down the **capability boundary** of the generic
// Webhook template.
//
// This is the only place in the entire package where "strings
// provided by the user will be evaluated as code", so it is
// necessary to make clear what it can and cannot do, and use tests
// to pin those properties — otherwise someday someone casually
// adds a method to the template context, or adds a readFile to the
// FuncMap, and the capability surface silently expands, while the
// diff looks like just a harmless little function.

// TestTemplateContextHasNoMethods is the most important one.
//
// text/template calls exported methods ({{.Foo}} can both fetch
// fields and call methods). So as long as the template context can
// touch **any** type that has exported methods, it is equivalent to
// exposing those methods to the template author. The context here
// is deliberately all pure data (only exported fields, zero
// methods).
//
// If this fails: someone has added a method to webhookTemplateData
// / webhookItem. Before letting it through, think about whether
// that method could be used by the template to read something not
// meant to be exposed.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d methods (%s): text/template can call them, "+
				"which is equivalent to opening those methods' capabilities up to the template author", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal locks in the set of functions exposed
// to the template.
//
// Each additional function in the FuncMap adds a capability. We
// currently only have json / jsons, whose role is to serialize
// values into JSON fragments — no file reading, no requests, no
// command execution.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("template function set changed: got %v, expected %v. Before adding a new function please confirm it does not expand the capability surface (no file read/write, no network requests, no command execution)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds access in
// templates: accessing something that does not exist must fail,
// rather than echo something; and the failure message must not leak
// internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a non-existent field should error")
	}
	// The error must not contain the real content from the
	// template context (finding title/summary).
	for _, leak := range []string{"SQL injection", "Parameter id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently — a bad template is a config
// error and retry won't self-heal. If it were judged retryable, a
// bad template would cause every delivery to whitewash through
// three rounds of backoff for nothing.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("template syntax errors should be blocked at save time")
	}
	// Even if validation is bypassed and delivery is attempted
	// directly, it must be judged a permanent failure, not retried
	// repeatedly.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a bad template should be judged a permanent failure, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the constraint that "the
// template render result must be valid JSON". It also incidentally
// blocks "use the template to generate plain text to trigger some
// other protocol" usages.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// A valid template passes.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("valid template should pass validation: %v", err)
	}
	// A non-JSON render must be rejected (not sent as-is).
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a non-JSON render should be a permanent failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("error message should explain it is a JSON issue, got %v", err)
	}
}
