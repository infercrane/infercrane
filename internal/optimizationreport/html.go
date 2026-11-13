package optimizationreport

import (
	"bytes"
	"html/template"
)

// HTML renders the report as one self-contained page: no scripts, fonts, or
// network requests. Outcome cards come first, then the measurement detail,
// the bottleneck signals when the engine reported any, and the evidence.
func HTML(r Report) ([]byte, error) {
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, newView(r)); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

var pageTemplate = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Optimization result</title>
<style>
:root{--bg:#f7f7f5;--panel:#fff;--ink:#17181a;--muted:#5d6168;--line:#e2e3e0;--good:#0f7b4a;--good-bg:#e6f4ec;--bad:#b42318;--bad-bg:#fdecea;--watch:#9a6700;--watch-bg:#fff4d6;--neutral:#3b4048;--neutral-bg:#eef0f2;--banner:#7a2e00;--banner-bg:#ffe8cc}
@media (prefers-color-scheme:dark){:root{--bg:#121315;--panel:#1b1d20;--ink:#ecedee;--muted:#a3a7ad;--line:#2c2f33;--good:#5fd39a;--good-bg:#12301f;--bad:#ff8a80;--bad-bg:#3a1714;--watch:#f2c14e;--watch-bg:#33290f;--neutral:#c9ccd1;--neutral-bg:#26292d;--banner:#ffd0a1;--banner-bg:#4a2408}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
.banner{position:sticky;top:0;z-index:10;background:var(--banner-bg);color:var(--banner);font-weight:700;text-align:center;padding:10px 16px;border-bottom:1px solid var(--line)}
main{max-width:1080px;margin:0 auto;padding:28px 16px 48px}
.eyebrow{color:var(--muted);font-size:13px;margin:0 0 4px}
h1{font-size:24px;line-height:1.3;margin:0 0 24px}
h2{font-size:17px;margin:36px 0 12px}
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(240px,1fr));gap:12px}
.card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px 16px}
.card .label{color:var(--muted);font-size:13px}
.card .value{font-size:26px;font-weight:650;margin:2px 0;overflow-wrap:anywhere}
.card .was{color:var(--muted);font-size:13px}
.pill{display:inline-block;border-radius:999px;padding:1px 9px;font-size:12.5px;font-weight:600;margin-top:8px}
.good{color:var(--good);background:var(--good-bg)}.bad{color:var(--bad);background:var(--bad-bg)}.watch{color:var(--watch);background:var(--watch-bg)}.neutral{color:var(--neutral);background:var(--neutral-bg)}.unknown{color:var(--muted);background:var(--neutral-bg)}
.note{color:var(--muted);font-size:12.5px;margin-top:6px}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px 16px}
.panel p{margin:0 0 8px}.panel p:last-child{margin:0}
table{width:100%;border-collapse:collapse;font-size:14px}
th,td{text-align:left;padding:8px 10px;border-bottom:1px solid var(--line);vertical-align:top}
th{color:var(--muted);font-weight:600;font-size:12.5px}
tr:last-child td{border-bottom:0}
.scroll{overflow-x:auto}
dl{display:grid;grid-template-columns:max-content 1fr;gap:6px 16px;margin:0}
dt{color:var(--muted)}dd{margin:0;overflow-wrap:anywhere}
code,.mono{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px;overflow-wrap:anywhere}
.two{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:12px}
footer{color:var(--muted);font-size:12.5px;margin-top:32px}
</style>
</head>
<body>
{{if .Banner}}<div class="banner" role="alert">{{.Banner}}</div>{{end}}
<main>
<p class="eyebrow">{{.Title}} · {{.Subtitle}}</p>
<h1>{{.Headline}}</h1>

<section aria-label="Outcomes" class="cards">
{{range .Rows}}<div class="card">
<div class="label">{{.Label}}</div>
<div class="value">{{.Winner}}</div>
<div class="was">current setup: {{.Baseline}}</div>
<span class="pill {{.Tone}}">{{.Change}}{{if .Decision}} · {{.Decision}}{{end}}</span>
{{if .Noise}}<div class="note">{{.Noise}}</div>{{end}}
{{if .Reason}}<div class="note">{{.Reason}}</div>{{end}}
</div>
{{end}}</section>

<h2>How it was measured</h2>
<div class="panel">
{{range .Measurement}}<p>{{.}}</p>
{{end}}</div>

{{if .Candidates}}<h2>Every setup measured</h2>
<div class="panel scroll"><table>
<thead><tr><th>Setup</th><th>Result</th><th>Measured</th><th>Against your current setup</th></tr></thead>
<tbody>
{{range .Candidates}}<tr><td>{{.Label}}{{if .Role}}<br><span class="pill neutral">{{.Role}}</span>{{end}}</td><td>{{.Status}}</td><td>{{.Measured}}</td><td>{{if .Versus}}<span class="{{.Tone}}">{{.Versus}}</span>{{end}}</td></tr>
{{end}}</tbody></table></div>{{end}}

{{if .Signals}}<h2>Bottleneck signals</h2>
<div class="panel scroll"><p class="note">{{.SignalsNote}}</p><table>
<thead><tr><th>Engine metric</th><th>Statistic</th><th>Current setup</th><th>Winner</th></tr></thead>
<tbody>
{{range .Signals}}<tr><td class="mono">{{.Name}}</td><td>{{.Stat}}</td><td>{{.Baseline}}</td><td>{{.Winner}}</td></tr>
{{end}}</tbody></table></div>{{end}}

<h2>Appendix: setup and evidence</h2>
<div class="two">
{{if .Winner}}<div class="panel"><p><strong>{{.WinnerTitle}}</strong></p><dl>
{{range .Winner}}<dt>{{.Name}}</dt><dd>{{.Value}}</dd>
{{end}}</dl></div>{{end}}
{{if .Baseline}}<div class="panel"><p><strong>{{.BaselineTitle}}</strong></p><dl>
{{range .Baseline}}<dt>{{.Name}}</dt><dd>{{.Value}}</dd>
{{end}}</dl></div>{{end}}
</div>
<h2>Evidence digests</h2>
<div class="panel"><dl>
{{range .Evidence}}<dt>{{.Name}}</dt><dd class="mono">{{.Value}}</dd>
{{end}}</dl></div>
{{if .Unknown}}<h2>Unknown, never estimated</h2>
<div class="panel">
{{range .Unknown}}<p>{{.Value}}</p>
{{end}}</div>{{end}}
<footer>Report digest <span class="mono">{{.Digest}}</span>. Every number comes from persisted measurements; anything missing is shown as unknown.</footer>
</main>
</body>
</html>
`))
