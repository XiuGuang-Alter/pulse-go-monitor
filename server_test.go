package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAPILifecycle(t *testing.T) {
	store := testStore(t)
	scheduler, _ := NewScheduler(store, 2, func(ctx context.Context, url string, timeout time.Duration) Result {
		return Result{URL: url, StatusCode: 200, Kind: "ok", Detail: "通过"}
	})
	defer scheduler.Stop()
	handler := NewApp(store, scheduler).Handler()
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, bytes.NewReader(data))
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	created := request("POST", "/api/targets", inputFor("https://example.com"))
	if created.Code != 201 {
		t.Fatal(created.Code, created.Body.String())
	}
	var target Target
	json.Unmarshal(created.Body.Bytes(), &target)
	path := "/api/targets/1"
	if w := request("POST", path+"/check", nil); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	await(t, func() bool {
		records, _ := store.History(context.Background(), target.ID, 10, 0)
		return len(records) == 1
	})
	for _, route := range []string{"/api/targets", path + "/checks?limit=1", "/api/status", "/", "/app.js", "/style.css"} {
		w := request("GET", route, nil)
		if w.Code != 200 {
			t.Errorf("%s: %d %s", route, w.Code, w.Body.String())
		}
	}
	changed := inputFor("https://example.com/new")
	changed.Enabled = false
	if w := request("PUT", path, changed); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, route := range []string{path + "/checks?limit=0", path + "/checks?before_id=-1", path + "/checks?limit=no"} {
		if w := request("GET", route, nil); w.Code != 400 {
			t.Error(route, w.Code)
		}
	}
	if w := request("DELETE", path, nil); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("GET", path+"/checks", nil); w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", "/api/targets", map[string]any{"name": "bad"}); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestStatusDiagnostics(t *testing.T) {
	store := testStore(t)
	s, _ := NewScheduler(store, 1, nil)
	defer s.Stop()
	w := httptest.NewRecorder()
	NewApp(store, s).Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8080/api/status", nil))
	var status map[string]any
	json.Unmarshal(w.Body.Bytes(), &status)
	if status["goroutines"] == nil || status["heap_alloc_bytes"] == nil {
		t.Fatal("missing process diagnostics", status)
	}
}

func TestAPIRejectsCrossSiteAndInvalidJSON(t *testing.T) {
	store := testStore(t)
	scheduler, _ := NewScheduler(store, 1, nil)
	defer scheduler.Stop()
	handler := NewApp(store, scheduler).Handler()
	for _, tc := range []struct {
		host, origin, contentType, body string
		want                            int
	}{
		{"127.0.0.1:8080", "https://evil.example", "application/json", `{}`, 403},
		{"evil.example:8080", "", "application/json", `{}`, 403},
		{"127.0.0.1:8080", "", "text/plain", `{}`, 415},
		{"127.0.0.1:8080", "", "application/json", `{} {}`, 400},
		{"127.0.0.1:8080", "", "application/json", `{"unexpected":1}`, 400},
	} {
		r := httptest.NewRequest("POST", "http://"+tc.host+"/api/targets", bytes.NewBufferString(tc.body))
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Content-Type", tc.contentType)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%+v -> %d %s", tc, w.Code, w.Body.String())
		}
	}
}

func TestServeArguments(t *testing.T) {
	for _, args := range [][]string{{"-addr", "0.0.0.0:8080"}, {"-concurrency", "0"}, {"unexpected"}} {
		var out bytes.Buffer
		if code := runServe(context.Background(), args, &out, &out); code != 2 {
			t.Fatalf("%v exit=%d %s", args, code, &out)
		}
	}
}
