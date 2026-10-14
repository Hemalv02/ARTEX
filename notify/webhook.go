package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel is a generic Webhook adapter: the user customizes the URL,
// method, request headers, and JSON template. It exists so this feature does not
// need a separate implementation for Slack / Mattermost / Discord / self-hosted
// systems — all of those can be covered by one configurable template.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// A generic Webhook has no official limit; returning 0 means no rate limiting by
// default, left for the user to set according to the receiver's capacity.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// Mask url and headers: the target URL itself often carries a token, and custom
// headers usually hold auth credentials; both appear in the API echo, so both
// must be blocked.
// The cost is that to change one header while editing, you have to refill the
// whole header set (a masked value is interpreted as "keep the original") — this
// trade-off is deliberate: better to refill once than to echo credentials back
// to the browser.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// The destination is url. When changing url you must re-confirm headers —
// otherwise the original Authorization header would be sent as-is to the new URL,
// which is precisely the main path for bypassing masking.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate is the fallback request body when no template is filled
// in: a plain JSON structure that covers the vast majority of self-hosted
// receivers that "take one JSON and store it".
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData is the context exposed to the user's template.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt is the time of this delivery (RFC3339), for the receiver to record.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel is a human-readable description of the status change, e.g.
	// "Pending → Fixed"; empty for non-status-change items.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("missing target URL")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("invalid target URL: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("unsupported method %s (allowed: GET/POST/PUT/PATCH)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("request body template syntax error: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET carries no request body: cramming the content into the query string is
	// beyond what the template can do and does not fit GET semantics, so GET only
	// suits receivers of the "trigger a hook on match" kind.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// The template renders JSON in string form; convert it to json.RawMessage
		// and send it as-is, to avoid double-escaping wrapping the user's
		// carefully built structure inside a single JSON string.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("request body template rendered to invalid JSON"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// Allow overriding, but apply it after headers so explicit config wins.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// A generic Webhook does not truncate the body (the receiver is the user's own
	// service, and the size is determined by body_template), so the whole batch
	// counts as delivered.
	return len(m.Items), nil
}

// renderWebhookBody renders the request body using the user's template (or the
// default template).
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("request body template syntax error: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("failed to render request body template: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate parses the template.
//
// missingkey=zero makes a missing map key render as the zero value instead of
// erroring — but the context in this file is a struct, so its main effect is to
// keep range from erroring when .Items is empty. What really needs guarding is
// .Items being nil.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs are the helper functions exposed to the template.
var webhookTemplateFuncs = template.FuncMap{
	// json serializes any value to JSON.
	//
	// This function is not a nicety but a necessity: without it, users could only
	// write {{.Title}} to interpolate directly, and as soon as a finding title
	// contains a quote or a newline the whole request body is no longer valid
	// JSON — the receiver rejects it, and the error message points to "JSON parse
	// failed", with no hint whatsoever that there was a quote in the title.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons is used to embed a JSON fragment inside another JSON string value (it
	// applies one layer of string escaping).
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// Strip the outer quotes: the caller decides whether to add quotes.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
