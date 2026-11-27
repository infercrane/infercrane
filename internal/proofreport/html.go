package proofreport

import (
	"bytes"
	"html/template"
)

// HTML renders the report as one self-contained page, ready to print to PDF:
// no scripts, fonts, images or network requests, A4 pages, and each workload
// starting on a new page when printed. html/template escapes every value.
func HTML(d Document) ([]byte, error) {
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, NewView(d)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

var pageTemplate = template.Must(template.New("proof").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style>
:root{--bg:#f7f7f5;--panel:#fff;--ink:#17181a;--muted:#5d6168;--line:#e2e3e0;--good:#0f7b4a;--good-bg:#e6f4ec;--bad:#b42318;--bad-bg:#fdecea;--watch:#9a6700;--watch-bg:#fff4d6;--neutral:#3b4048;--neutral-bg:#eef0f2}
@media screen and (prefers-color-scheme:dark){:root{--bg:#121315;--panel:#1b1d20;--ink:#ecedee;--muted:#a3a7ad;--line:#2c2f33;--good:#5fd39a;--good-bg:#12301f;--bad:#ff8a80;--bad-bg:#3a1714;--watch:#f2c14e;--watch-bg:#33290f;--neutral:#c9ccd1;--neutral-bg:#26292d}}
@page{size:A4;margin:16mm 14mm}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
main{max-width:1040px;margin:0 auto;padding:28px 16px 48px}
.eyebrow{color:var(--muted);font-size:13px;margin:0 0 4px}
h1{font-size:26px;line-height:1.25;margin:0 0 8px}
h2{font-size:19px;margin:36px 0 8px}
h3{font-size:15px;margin:24px 0 8px}
.lead{font-size:17px;font-weight:600;margin:12px 0 20px}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin:0 0 12px}
.panel p{margin:0 0 8px}.panel p:last-child{margin:0}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{text-align:left;padding:7px 9px;border-bottom:1px solid var(--line);vertical-align:top}
td{overflow-wrap:anywhere;min-width:5.5em}
td:first-child,th{white-space:nowrap}
p,li{overflow-wrap:anywhere}
th{color:var(--muted);font-weight:600;font-size:12px}
tr:last-child td{border-bottom:0}
dl{display:grid;grid-template-columns:max-content 1fr;gap:6px 16px;margin:0}
dt{color:var(--muted)}dd{margin:0;overflow-wrap:anywhere}
.mono,pre{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px;overflow-wrap:anywhere}
pre{white-space:pre-wrap;margin:0}
.pill{display:inline-block;border-radius:999px;padding:1px 9px;font-size:12px;font-weight:600}
.good{color:var(--good);background:var(--good-bg)}.bad{color:var(--bad);background:var(--bad-bg)}.watch{color:var(--watch);background:var(--watch-bg)}.neutral{color:var(--neutral);background:var(--neutral-bg)}.unknown{color:var(--muted);background:var(--neutral-bg)}
.note{color:var(--muted);font-size:12.5px}
.warning{color:var(--watch);background:var(--watch-bg);border:1px solid var(--line);border-radius:10px;padding:8px 12px;margin:0 0 10px}
.workload{break-before:page}
.workload:first-of-type{break-before:auto}
.run{break-inside:avoid-page}
footer{color:var(--muted);font-size:12px;margin-top:32px}
footer p{margin:0 0 6px}
@media print{body{background:#fff}.panel{border-color:#ccc}main{max-width:none;padding:0}}
</style>
</head>
<body>
<main>
<p class="eyebrow">{{.Subtitle}}</p>
<h1>{{.Title}}</h1>
<p class="lead">{{.Headline}}</p>
<section class="panel" aria-label="Summary"><dl>
{{range .Facts}}<dt>{{.Name}}</dt><dd>{{.Value}}</dd>
{{end}}</dl></section>
<section class="panel" aria-label="How to read this report"><p><strong>How to read this report.</strong> {{.ClaimRule}} A number marked unknown was not measured or not comparable; nothing is estimated.</p></section>
{{range .Workloads}}<section class="workload" aria-label="{{.Heading}}">
<h2>{{.Heading}}</h2>
<p>{{.Summary}}</p>
<p class="note">Traffic: {{.Traffic}}.</p>
{{if .Saving}}<div class="panel"><dl>{{range .Saving}}<dt>{{.Name}}</dt><dd>{{.Value}}</dd>{{end}}</dl></div>{{end}}
{{if or .Loads .LoadNote}}<h3>Verdict per load</h3>
{{if .LoadNote}}<p>{{.LoadNote}}</p>{{end}}
{{if .Loads}}<div class="panel"><table><thead><tr><th>Load</th><th>Decision</th><th>Setup</th><th>Why</th></tr></thead><tbody>
{{range .Loads}}<tr><td>{{.Load}}</td><td>{{.Decision}}</td><td>{{.Setup}}</td><td>{{.Reason}}</td></tr>
{{end}}</tbody></table></div>{{end}}{{end}}
{{range .Runs}}<article class="run">
<h3>{{.Heading}}</h3>
<p><strong>{{.Headline}}</strong></p>
{{range .Warnings}}<p class="warning" role="note">Check: {{.}}</p>{{end}}
{{if .Setups}}<div class="panel"><table><thead><tr><th>Measured on</th><th>Before (your current setup)</th><th>After</th></tr></thead><tbody>
{{range .Setups}}<tr><td>{{.Name}}</td><td>{{.Before}}</td><td>{{.After}}</td></tr>
{{end}}</tbody></table></div>{{end}}
<div class="panel"><dl>{{range .Context}}<dt>{{.Name}}</dt><dd>{{.Value}}</dd>{{end}}</dl></div>
{{if .Numbers}}<div class="panel"><table><thead><tr><th>Outcome</th><th>Before</th><th>After</th><th>Change</th><th>Noise</th><th>Decision</th></tr></thead><tbody>
{{range .Numbers}}<tr><td>{{.Label}}</td><td>{{.Before}}</td><td>{{.After}}</td><td>{{.Change}}</td><td>{{.Noise}}</td><td><span class="pill {{.Tone}}">{{.Decision}}</span>{{if .Reason}}<div class="note">{{.Reason}}</div>{{end}}</td></tr>
{{end}}</tbody></table></div>{{end}}
{{if .Targets}}<div class="panel"><table><thead><tr><th>Your limit</th><th>Before</th><th>After</th></tr></thead><tbody>
{{range .Targets}}<tr><td>{{.Limit}}</td><td>{{.Before}}</td><td>{{.After}}</td></tr>
{{end}}</tbody></table></div>{{end}}
<div class="panel">
<p><strong>Answer quality.</strong> {{.Quality}}</p>
<p><span class="pill {{.ClaimTone}}">{{.Claim}}</span></p>
<p><span class="pill {{.Tone}}">{{.Saving}}</span></p>
{{if .Reasons}}<ul>{{range .Reasons}}<li>{{.}}</li>{{end}}</ul>{{end}}
</div>
<div class="panel"><p><strong>Evidence</strong></p><dl>{{range .Evidence}}<dt>{{.Name}}</dt><dd class="mono">{{.Value}}</dd>{{end}}</dl></div>
{{if .Rerun}}<div class="panel"><p><strong>Measure it again</strong></p><pre>{{range .Rerun}}{{.}}
{{end}}</pre></div>{{end}}
</article>
{{end}}</section>
{{end}}
<h2>Limits</h2>
<div class="panel"><ul>{{range .Limits}}<li>{{.}}</li>{{end}}</ul></div>
{{if .Unknown}}<h2>Unknown, never estimated</h2>
<div class="panel"><ul>{{range .Unknown}}<li>{{.}}</li>{{end}}</ul></div>{{end}}
<footer><p>Report digest <span class="mono">{{.Digest}}</span>. Every number comes from the runs' persisted measurements.</p>
<p>{{.Signature}}</p></footer>
</main>
</body>
</html>
`))
