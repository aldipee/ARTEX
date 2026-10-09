package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// These tests preserve the **capability boundary** of generic Webhook templates.
//
// This is the only place in the package where user-provided strings are evaluated
// as code. Tests make the limits explicit: adding a method to the template context
// or a readFile function to FuncMap could silently expand its capabilities.

// TestTemplateContextHasNoMethods is the most important boundary check.
//
// text/template invokes exported methods as well as reading fields, so exposing
// any type with exported methods would expose them to template authors. This
// context intentionally contains only data fields and no methods.
//
// A failure means someone added a method to webhookTemplateData or webhookItem.
// Review whether it could expose data before allowing that change.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d methods (%s); text/template can call them, " +
				"giving template authors access to their capabilities", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal preserves the small set of functions exposed to templates.
//
// Every added FuncMap function expands the template's capabilities. Only json/jsons
// are exposed to serialize values; templates cannot read files, make requests, or run commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("template function set changed: got %v, want %v. Before adding a function, confirm it does not expand capabilities " +
			"(no file access, network requests, or command execution)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers out-of-bounds template access: missing
// fields must fail without echoing internal data in the error.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a missing field should return an error")
	}
	// The error must not include real context values such as finding titles or summaries.
	for _, leak := range []string{"SQL injection", "Parameter id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently ensures a template syntax error is permanent;
// retrying a bad template would waste three backoff attempts on every delivery.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("template syntax errors should be rejected when saving")
	}
	// Even when validation is bypassed, delivery should fail permanently rather than retry.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("bad template should be classified as a permanent failure; got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON enforces valid JSON output and prevents using
// templates to produce plain text for another protocol.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// A valid template should pass.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("valid template should pass validation: %v", err)
	}
	// Reject non-JSON output instead of sending it as-is.
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("non-JSON output should be a permanent failure; got %v", err)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("error should identify the JSON problem; got %v", err)
	}
}
