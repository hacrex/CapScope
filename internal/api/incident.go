package api

// Self-contained HTML incident export (Phase 6.3 / PRD): one file, no network
// fetches, no external fonts or scripts — just the crash evidence styled with
// inline CSS and inline SVG sparklines, ready to drop into a ticket or share.

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"capscope/internal/store"
)

func parseID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (s *Server) snapshotIncident(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		fail(w, 400, err)
		return
	}
	sn, err := s.snapshotEnriched(r.Context(), id)
	if err != nil {
		fail(w, 404, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="incident-%s-%d.html"`, sn.App, sn.ID))
	_ = incidentTmpl.Execute(w, incidentData(sn))
}

// incidentWrap is a snapshot plus the derived view data the template needs.
type incidentWrap struct {
	store.Snapshot
	When       string
	Generated  string
	CPUSpark   template.HTML // inline SVG polyline, "" when no metrics
	MemSpark   template.HTML // inline SVG polyline over memory % of limit
	MemMax     float64
	IsOOM      bool
	DeployLine string
}

func incidentData(sn store.Snapshot) incidentWrap {
	d := incidentWrap{
		Snapshot:  sn,
		When:      time.UnixMilli(sn.TS).UTC().Format("2006-01-02 15:04:05 UTC"),
		Generated: time.Now().UTC().Format("2006-01-02 15:04 UTC"),
		IsOOM:     sn.OOMKilled || sn.Kind == "oom",
	}
	d.CPUSpark, d.MemSpark, d.MemMax = sparklines(sn.Metrics)
	if sn.Deploy != nil {
		gap := (sn.TS - sn.Deploy.TS) / 60000
		d.DeployLine = fmt.Sprintf("%s at %s (%d min before the crash)",
			sn.Deploy.Version, time.UnixMilli(sn.Deploy.TS).UTC().Format("15:04:05"), gap)
	}
	return d
}

// sparklines render cpu% and mem%-of-limit trends as SVG polyline point lists
// inside a 480x100 box. No JS needed; template.HTML is intentional here.
func sparklines(m []store.MetricPoint) (cpu, mem template.HTML, memMax float64) {
	if len(m) < 2 {
		return "", "", 0
	}
	const w = 480.0
	var cpuPts []string
	var memPts []string
	// Memory scale: peak of % of limit (or absolute MiB when no limit known).
	peak := 0.0
	abs := false
	for _, p := range m {
		var pct float64
		if p.MemLimit > 0 {
			pct = float64(p.Mem) / float64(p.MemLimit) * 100
		} else {
			abs = true
			pct = float64(p.Mem) / 1048576 // MiB
		}
		if pct > peak {
			peak = pct
		}
	}
	peak = max(peak, 1)
	for i, p := range m {
		x := fmt.Sprintf("%.1f", float64(i)*w/float64(len(m)-1))
		cpuPts = append(cpuPts, x+","+fmt.Sprintf("%.1f", 100-p.CPU))
		var pct float64
		if !abs && p.MemLimit > 0 {
			pct = float64(p.Mem) / float64(p.MemLimit) * 100
		} else {
			pct = float64(p.Mem) / 1048576
		}
		memPts = append(memPts, x+","+fmt.Sprintf("%.1f", peak-pct))
	}
	memScale := "% of limit"
	if abs {
		memScale = "MiB"
	}
	cpu = template.HTML(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="#0e7490" stroke-width="1.5"/></svg><span class="u">cpu %%</span>`,
		strings.Join(cpuPts, " ")))
	mem = template.HTML(fmt.Sprintf(`<polyline points="%s" fill="none" stroke="#9333ea" stroke-width="1.5"/></svg><span class="u">memory (%s, peak %.0f)</span>`,
		strings.Join(memPts, " "), memScale, peak))
	return cpu, mem, memMax
}

const incidentTmplText = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<title>Incident — crash in {{.App}}</title>
<style>
 body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;margin:2rem auto;max-width:900px;color:#1a1a1a;background:#fff;line-height:1.5}
 h1{font-size:1.4rem;margin:0 0 .2em} h2{font-size:1.05rem;border-bottom:1px solid #e5e5e5;padding-bottom:.25em;margin-top:2em}
 table{border-collapse:collapse;font-size:.9rem} td{padding:3px 14px 3px 0;vertical-align:top}
 td:first-child{color:#666;white-space:nowrap} code,pre{font-family:ui-monospace,Consolas,monospace}
 .card{border:1px solid #e5e5e5;border-radius:8px;padding:12px 16px;margin:1rem 0}
 .muted{color:#777;font-size:.8rem}
 .pre{background:#f7f7f7;border:1px solid #e5e5e5;border-radius:8px;padding:10px 14px;max-height:22em;overflow:auto;font-size:.78rem;white-space:pre-wrap;word-break:break-word}
 svg{background:#fafafa;border:1px solid #eee;border-radius:6px}
 .u{font-size:.75rem;color:#666;margin-left:8px}
 .oom{color:#b91c1c;font-weight:600} .ok{color:#047857}
 footer{margin-top:3rem;border-top:1px solid #e5e5e5;padding-top:.8rem;font-size:.75rem;color:#999}
</style></head><body>
<h1>Crash in <code>{{.App}}</code></h1>
<p class="muted">at {{.When}} &middot; snapshot #{{.ID}} &middot; self-contained export generated {{.Generated}} by CapScope</p>

<table>
 <tr><td>Container</td><td><code>{{.Container}}</code> &middot; <code>{{.Image}}</code></td></tr>
 <tr><td>Outcome</td><td>{{if .IsOOM}}<span class="oom">OOM killed</span>{{else}}<span class="ok">exit code {{.ExitCode}}</span>{{end}}{{if .RestartCount}} &middot; restart count {{.RestartCount}}{{end}}{{if ge .RestartsWindow 2}} &middot; {{.RestartsWindow}} deaths in the restart window{{end}}</td></tr>
 <tr><td>Uptime</td><td>{{.UptimeSec}}s before the crash{{if .MemLimit}} &middot; memory limit {{.MemLimit}} bytes{{end}}</td></tr>
{{if .Error}}<tr><td>Docker error</td><td><code>{{.Error}}</code></td></tr>{{end}}
{{if .DeployLine}}<tr><td>Deploy before crash</td><td>{{.DeployLine}}</td></tr>{{end}}
</table>

<h2>Summary</h2>
<div class="card">{{.Summary}}</div>

{{if .Metrics}}
<h2>Resource trend before the crash</h2>
<p>
 <svg width="480" height="100" viewBox="0 0 480 100">{{.CPUSpark}}
 <svg width="480" height="100" viewBox="0 0 480 100">{{.MemSpark}}
</p>
{{end}}

<h2>Last {{len .Logs}} log lines</h2>
<div class="pre">{{range .Logs}}{{fmtLog .}}{{end}}</div>

<footer>CapScope — read-only Docker log observer. Attach this file to the incident ticket; recipients need no access to the instance.</footer>
</body></html> `

// incidentTmpl registers the fmt/err helpers once; html/template escapes all
// interpolations automatically, so log text cannot inject markup.
func init() {
	t := template.New("incident").Funcs(template.FuncMap{
		"fmtLog": func(l store.LogRow) string {
			st := "out"
			if l.Stream == "e" {
				st = "err"
			}
			return fmt.Sprintf("%s %s %s\n", time.UnixMilli(l.TSMs).UTC().Format("15:04:05.000"), st, l.Line)
		},
	})
	incidentTmpl = template.Must(t.Parse(incidentTmplText))
}

var incidentTmpl *template.Template
