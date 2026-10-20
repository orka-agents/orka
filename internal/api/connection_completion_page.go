/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"html/template"

	"github.com/gofiber/fiber/v3"
	"k8s.io/apimachinery/pkg/util/validation"
)

// connectorCompletionReasons are the reason codes ConnectionCallback puts in
// its redirect. Anything else is not echoed, so a crafted link cannot show
// its own words as the failure.
var connectorCompletionReasons = map[string]bool{
	"access_denied":          true, //nolint:goconst // Wire reason codes stay literal so the list reads against the callback.
	"connection_mismatch":    true,
	"connection_missing":     true,
	"connection_unprotected": true,
	"consent_expired":        true,
	"consent_superseded":     true,
	"disconnected":           true,
	"exchange_failed":        true,
	"invalid_state":          true,
	"missing_code":           true,
	"mode_changed":           true,
	"provider_changed":       true,
	"provider_rejected":      true,
	"provider_unavailable":   true,
	"scopes_denied":          true,
	"storage_failed":         true,
}

// connectorCompletionTemplate is the page the connector callback lands on
// when the controller serves no dashboard. It runs no script and loads
// nothing: the one-time completion value stays in the URL fragment, which
// never reaches the server, and the person copies it into the CLI.
var connectorCompletionTemplate = template.Must(template.New("connector-completion").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Orka account link</title>
</head>
<body>
{{- if .Failed}}
<h1>Linking did not finish</h1>
<p>{{if .Reason}}Linking failed ({{.Reason}}).{{else}}Linking failed.{{end}}</p>
<p>Start the link again with <code>orka connect &lt;provider&gt;</code>.</p>
{{- else}}
<h1>Finish linking your account</h1>
<p>This Orka server does not run the dashboard, so finish the link with the CLI. Copy the value after <code>#completion=</code> in this page's address bar and run:</p>
<pre>{{.Command}}</pre>
{{- if .UnknownName}}
<p><code>orka connection list</code> shows the link's name.</p>
{{- end}}
<p>The value works once. Do not share it.</p>
{{- end}}
</body>
</html>
`))

type connectorCompletionView struct {
	Failed      bool
	Reason      string
	Command     string
	UnknownName bool
}

// ConnectorCompletionPage serves the connector callback's redirect target
// when the dashboard is disabled. The query is attacker-controlled, so only
// valid Kubernetes names and reason codes are shown; anything else falls
// back to a placeholder rather than a command someone else wrote.
func (h *Handlers) ConnectorCompletionPage(c fiber.Ctx) error {
	view := connectorCompletionView{Failed: c.Query("status") == "error"}
	if reason := c.Query("reason"); connectorCompletionReasons[reason] {
		view.Reason = reason
	}
	name := c.Query("connection")
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		name, view.UnknownName = "<name>", true
	}
	view.Command = "orka connection complete " + name
	if namespace := c.Query("namespace"); namespace != "" && len(validation.IsDNS1123Label(namespace)) == 0 {
		view.Command += " --namespace " + namespace
	}
	view.Command += " --completion <value>"

	var page bytes.Buffer
	if err := connectorCompletionTemplate.Execute(&page, view); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to render the completion page")
	}
	c.Set(fiber.HeaderContentType, "text/html; charset=utf-8")
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderReferrerPolicy, "no-referrer")
	c.Set(fiber.HeaderContentSecurityPolicy, "default-src 'none'; frame-ancestors 'none'")
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	return c.Status(fiber.StatusOK).Send(page.Bytes())
}
