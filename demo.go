package main

import (
	"html/template"
	"net/http"
	"sync/atomic"
)

var demoScenarioPage = template.Must(template.New("scenario").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Pulse · 故障与恢复演示</title>
  <style>
    body { font: 1rem/1.6 system-ui, sans-serif; color: #17233b; background: #f4f7fc; margin: 0; }
    main { max-width: 35rem; margin: 8vh auto; padding: 1.5rem; }
    h1 { margin-bottom: .5rem; line-height: 1.2; }
    form { display: flex; flex-wrap: wrap; gap: .75rem; margin: 1.25rem 0; }
    button { font: inherit; background: #284fb9; color: #fff; border: 0; border-radius: .4rem; padding: .55rem 1rem; cursor: pointer; }
    button:disabled { background: #e2e8f5; color: #344158; cursor: default; }
  </style>
</head>
<body><main>
  <h1>故障与恢复演示</h1>
  <p>「演示 · 故障与恢复」当前返回 <strong>{{if .}}HTTP 503{{else}}HTTP 200{{end}}</strong>。</p>
  <form method="post">
    <button formaction="/demo/fail" {{if .}}disabled{{end}}>模拟故障</button>
    <button formaction="/demo/recover" {{if not .}}disabled{{end}}>恢复服务</button>
  </form>
  <ol>
    <li>点「模拟故障」，等 Pulse 保存两次失败。</li>
    <li>在监测控制台打开「故障过程」，确认故障已记录。</li>
    <li>点「恢复服务」，等下一次成功后点「查看检查」。</li>
  </ol>
</main></body>
</html>`))

func registerDemoScenario(mux *http.ServeMux) {
	var failed atomic.Bool
	mux.HandleFunc("GET /switch", func(w http.ResponseWriter, _ *http.Request) {
		if failed.Load() {
			jsonResponse(w, http.StatusServiceUnavailable, map[string]string{"message": "模拟服务不可用"})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"message": "演示服务正常"})
	})
	mux.HandleFunc("GET /demo", func(w http.ResponseWriter, r *http.Request) {
		if !loopbackAddress(r.Host) {
			http.Error(w, "仅允许本机访问", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		_ = demoScenarioPage.Execute(w, failed.Load())
	})
	setFailure := func(fail bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !loopbackAddress(r.Host) || r.Header.Get("Origin") != "http://"+r.Host {
				http.Error(w, "仅允许演示控制页切换状态", http.StatusForbidden)
				return
			}
			failed.Store(fail)
			http.Redirect(w, r, "/demo", http.StatusSeeOther)
		}
	}
	mux.HandleFunc("POST /demo/fail", setFailure(true))
	mux.HandleFunc("POST /demo/recover", setFailure(false))
}
