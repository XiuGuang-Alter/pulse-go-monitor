package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var webAssets embed.FS

type App struct {
	store     *Store
	scheduler *Scheduler
	startedAt time.Time
}

func NewApp(store *Store, scheduler *Scheduler) *App {
	return &App{store: store, scheduler: scheduler, startedAt: time.Now()}
}
func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
func apiError(w http.ResponseWriter, err error) {
	status := 500
	message := "服务暂时无法完成请求，请查看服务日志"
	switch {
	case errors.Is(err, ErrNotFound):
		status = 404
		message = err.Error()
	case errors.Is(err, ErrValidation), errors.Is(err, ErrLimit):
		status = 400
		message = err.Error()
	case errors.Is(err, ErrBusy):
		status = 409
		message = err.Error()
	case errors.Is(err, ErrStopped):
		status = 503
		message = err.Error()
	default:
		log.Printf("API错误: %v", err)
	}
	jsonResponse(w, status, map[string]string{"error": message})
}
func decodeInput(w http.ResponseWriter, r *http.Request) (TargetInput, error) {
	var in TargetInput
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return in, errors.Join(ErrValidation, errors.New("JSON正文无效、字段未知或内容过大"))
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return in, errors.Join(ErrValidation, errors.New("正文只能包含一个JSON对象"))
	}
	return in, nil
}
func targetID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrValidation
	}
	return id, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		if err := a.store.db.PingContext(r.Context()); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, map[string]any{"status": "ok", "started_at": a.startedAt.UnixMilli(), "server_time": time.Now().UnixMilli(), "running": len(a.scheduler.RunningIDs()), "concurrency": cap(a.scheduler.slots)})
	})
	mux.HandleFunc("GET /api/targets", func(w http.ResponseWriter, r *http.Request) {
		targets, err := a.store.ListTargets(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		running := a.scheduler.RunningIDs()
		for i := range targets {
			targets[i].Running = running[targets[i].ID]
		}
		jsonResponse(w, 200, targets)
	})
	mux.HandleFunc("POST /api/targets", func(w http.ResponseWriter, r *http.Request) {
		in, err := decodeInput(w, r)
		if err != nil {
			apiError(w, err)
			return
		}
		target, err := a.store.SaveTarget(r.Context(), 0, in)
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 201, target)
		a.scheduler.Notify()
	})
	mux.HandleFunc("PUT /api/targets/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := targetID(r)
		if err != nil {
			apiError(w, err)
			return
		}
		in, err := decodeInput(w, r)
		if err != nil {
			apiError(w, err)
			return
		}
		target, err := a.store.SaveTarget(r.Context(), id, in)
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, target)
		a.scheduler.Notify()
	})
	mux.HandleFunc("DELETE /api/targets/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := targetID(r)
		if err != nil {
			apiError(w, err)
			return
		}
		if err = a.store.DeleteTarget(r.Context(), id); err != nil {
			apiError(w, err)
			return
		}
		w.WriteHeader(204)
		a.scheduler.Notify()
	})
	mux.HandleFunc("POST /api/targets/{id}/check", func(w http.ResponseWriter, r *http.Request) {
		id, err := targetID(r)
		if err != nil {
			apiError(w, err)
			return
		}
		if err = a.scheduler.Trigger(id); err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 202, map[string]string{"message": "检查已开始"})
	})
	mux.HandleFunc("GET /api/targets/{id}/checks", func(w http.ResponseWriter, r *http.Request) {
		id, err := targetID(r)
		if err != nil {
			apiError(w, err)
			return
		}
		limit := 50
		var before int64
		if value := r.URL.Query().Get("limit"); value != "" {
			limit, err = strconv.Atoi(value)
			if err != nil {
				apiError(w, ErrValidation)
				return
			}
		}
		if value := r.URL.Query().Get("before_id"); value != "" {
			before, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				apiError(w, ErrValidation)
				return
			}
		}
		records, err := a.store.History(r.Context(), id, limit, before)
		if err != nil {
			apiError(w, err)
			return
		}
		var next int64
		if len(records) == limit {
			next = records[len(records)-1].ID
		}
		jsonResponse(w, 200, map[string]any{"items": records, "next_before_id": next})
	})
	mux.HandleFunc("GET /api/targets/{id}/daily", func(w http.ResponseWriter, r *http.Request) {
		id, err := targetID(r)
		if err != nil {
			apiError(w, err)
			return
		}
		days := 30
		if raw := r.URL.Query().Get("days"); raw != "" {
			days, err = strconv.Atoi(raw)
			if err != nil {
				apiError(w, ErrValidation)
				return
			}
		}
		data, err := a.store.Daily(r.Context(), id, days)
		if err != nil {
			apiError(w, err)
			return
		}
		jsonResponse(w, 200, data)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, 404, map[string]string{"error": "接口不存在"})
	})
	assets, _ := fs.Sub(webAssets, "web")
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			jsonResponse(w, 403, map[string]string{"error": "仅允许本机访问"})
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host || u.Scheme != "http" {
					jsonResponse(w, 403, map[string]string{"error": "不允许跨站修改"})
					return
				}
			}
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/check") {
				mux.ServeHTTP(w, r)
				return
			}
			if r.Method == "POST" || r.Method == "PUT" {
				media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if media != "application/json" {
					jsonResponse(w, 415, map[string]string{"error": "请使用 application/json"})
					return
				}
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}
