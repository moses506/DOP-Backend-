package mail

import (
	"bytes"
	"html/template"
	"strings"
)

// Content is a simple branded email: a heading, a few paragraphs, one button.
type Content struct {
	Preheader  string // inbox preview line
	Heading    string
	Paragraphs []string
	ButtonText string
	ButtonURL  string
	Details    [][2]string // label/value rows shown in a box, e.g. Email / Role
	After      []string    // paragraphs after the details, e.g. what to do next and a sign-off
	Footnote   string
}

// Render returns the HTML and plain-text bodies for c.
func Render(c Content) (html, text string) {
	var buf bytes.Buffer
	_ = layout.Execute(&buf, c)

	var t strings.Builder
	t.WriteString(c.Heading + "\n\n")
	for _, p := range c.Paragraphs {
		t.WriteString(p + "\n\n")
	}
	for _, d := range c.Details {
		t.WriteString(d[0] + ": " + d[1] + "\n")
	}
	if len(c.Details) > 0 {
		t.WriteString("\n")
	}
	for _, p := range c.After {
		t.WriteString(p + "\n\n")
	}
	if c.ButtonURL != "" {
		t.WriteString(c.ButtonText + ":\n" + c.ButtonURL + "\n\n")
	}
	if c.Footnote != "" {
		t.WriteString(c.Footnote + "\n\n")
	}
	t.WriteString("—\nDOS BPA · Digital Office Solutions Limited\nBusiness Solutions Beyond Borders\n")
	return buf.String(), t.String()
}

// Table-based layout with inline styles: the format email clients render reliably.
var layout = template.Must(template.New("email").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Heading}}</title></head>
<body style="margin:0;padding:0;background:#f2f4f8;font-family:'Plus Jakarta Sans',Segoe UI,Helvetica,Arial,sans-serif;color:#1d2433;">
<span style="display:none;max-height:0;overflow:hidden;opacity:0;">{{.Preheader}}</span>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f2f4f8;padding:32px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;">
  <tr><td style="background:#141b47;border-radius:16px 16px 0 0;padding:26px 32px;">
    <table role="presentation" cellpadding="0" cellspacing="0"><tr>
      <td style="padding-right:12px;vertical-align:middle;">
        <table role="presentation" cellpadding="0" cellspacing="0"><tr>
          <td style="width:12px;height:22px;background:#c9ccd3;border-radius:3px;"></td><td style="width:4px;"></td>
          <td style="width:12px;height:22px;background:#3d55dc;border-radius:3px;"></td><td style="width:4px;"></td>
          <td style="width:12px;height:22px;background:#8a8f99;border-radius:3px;"></td>
        </tr></table>
      </td>
      <td style="vertical-align:middle;">
        <div style="font-size:18px;font-weight:800;color:#ffffff;letter-spacing:-0.2px;">DOS BPA</div>
        <div style="font-size:10px;letter-spacing:1.6px;text-transform:uppercase;color:#aab1cc;">Digital Office Solutions</div>
      </td>
    </tr></table>
  </td></tr>
  <tr><td style="background:#ffffff;padding:36px 32px 28px;border-radius:0 0 16px 16px;">
    <h1 style="margin:0 0 16px;font-size:24px;line-height:1.25;color:#121838;letter-spacing:-0.4px;">{{.Heading}}</h1>
    {{range .Paragraphs}}<p style="margin:0 0 14px;font-size:15px;line-height:1.65;color:#4a5170;white-space:pre-line;">{{.}}</p>{{end}}
    {{if .Details}}
    <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="margin:18px 0 6px;border:1px solid #e2e4ea;border-radius:12px;">
      {{range $i, $d := .Details}}<tr>
        <td style="padding:11px 16px;font-size:13px;color:#6b7387;{{if $i}}border-top:1px solid #e2e4ea;{{end}}width:38%;">{{index $d 0}}</td>
        <td style="padding:11px 16px;font-size:14px;font-weight:600;color:#121838;{{if $i}}border-top:1px solid #e2e4ea;{{end}}">{{index $d 1}}</td>
      </tr>{{end}}
    </table>{{end}}
    {{range .After}}<p style="margin:16px 0 0;font-size:15px;line-height:1.65;color:#4a5170;white-space:pre-line;">{{.}}</p>{{end}}
    {{if .ButtonURL}}
    <table role="presentation" cellpadding="0" cellspacing="0" style="margin:26px 0 22px;"><tr>
      <td style="border-radius:12px;background:#2337b8;">
        <a href="{{.ButtonURL}}" style="display:inline-block;padding:15px 28px;font-size:15px;font-weight:700;color:#ffffff;text-decoration:none;border-radius:12px;">{{.ButtonText}} &rarr;</a>
      </td></tr></table>
    <p style="margin:0 0 14px;font-size:12.5px;line-height:1.6;color:#8a91a6;">If the button doesn't work, copy this link into your browser:<br>
      <a href="{{.ButtonURL}}" style="color:#2337b8;word-break:break-all;">{{.ButtonURL}}</a></p>
    {{end}}
    {{if .Footnote}}<p style="margin:18px 0 0;padding-top:16px;border-top:1px solid #eceef3;font-size:12.5px;line-height:1.6;color:#8a91a6;">{{.Footnote}}</p>{{end}}
  </td></tr>
  <tr><td style="padding:20px 8px;text-align:center;font-size:12px;line-height:1.6;color:#8a91a6;">
    Digital Office Solutions Limited · Lusaka, Zambia<br>Business Solutions Beyond Borders
  </td></tr>
</table>
</td></tr></table>
</body></html>`))
