package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// 若把 500 当成网络错误，或只接受 200 而拒绝 204，这个测试会失败。
func TestCheckHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   string
	}{{200, "ok"}, {204, "ok"}, {299, "ok"}, {300, "http_error"}, {500, "http_error"}} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %s, want GET", r.Method)
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			got := Check(context.Background(), server.URL, time.Second)
			if got.StatusCode != tc.status || got.Kind != tc.kind {
				t.Fatalf("Check() = %+v, want status %d, kind %s", got, tc.status, tc.kind)
			}
			if got.Duration < 0 || got.Detail == "" {
				t.Fatalf("missing duration or explanation: %+v", got)
			}
		})
	}
}

func TestCheckInvalidURL(t *testing.T) {
	for _, input := range []string{"", "/relative", "example.com", "http:///path", "http://:80", "ftp://example.com", "http://user:pass@example.com", "http://example.com:abc", "http://example.com:99999"} {
		t.Run(input, func(t *testing.T) {
			got := Check(context.Background(), input, time.Second)
			if got.Kind != "invalid_input" || got.StatusCode != 0 || got.Detail == "" {
				t.Fatalf("Check(%q) = %+v, want invalid_input without status", input, got)
			}
		})
	}
}

func TestCheckRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-ok":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/to-error":
			http.Redirect(w, r, "/error", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/error":
			w.WriteHeader(500)
		default:
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	for _, tc := range []struct {
		path, kind, finalPath string
		status                int
	}{{"/to-ok", "ok", "/ok", 200}, {"/to-error", "http_error", "/error", 500}, {"/loop", "request_error", "", 0}} {
		t.Run(tc.path, func(t *testing.T) {
			got := Check(context.Background(), server.URL+tc.path, time.Second)
			if got.Kind != tc.kind || got.StatusCode != tc.status {
				t.Fatalf("Check() = %+v, want %s/%d", got, tc.kind, tc.status)
			}
			if tc.finalPath != "" && got.URL != server.URL+tc.finalPath {
				t.Fatalf("final URL = %s, want %s", got.URL, server.URL+tc.finalPath)
			}
		})
	}
}

func TestCheckTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	start := time.Now()
	got := Check(context.Background(), server.URL, 50*time.Millisecond)
	if got.Kind != "timeout" || got.StatusCode != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("Check() = %+v, want timeout within 2s", got)
	}
}

func TestCheckCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a canceled check must not reach the handler")
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	got := Check(ctx, server.URL, time.Second)
	if got.Kind != "request_error" || got.StatusCode != 0 || time.Since(start) > time.Second {
		t.Fatalf("Check() = %+v, want prompt request_error", got)
	}
}

func TestCheckConnectionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	address := server.URL
	server.Close()
	got := Check(context.Background(), address, 4*time.Second)
	if got.Kind != "request_error" || got.StatusCode != 0 {
		t.Fatalf("Check() = %+v, want request_error without HTTP status", got)
	}
}

func TestCheckDoesNotWaitForBody(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer server.Close()
	defer close(release)
	got := Check(context.Background(), server.URL, 500*time.Millisecond)
	if got.Kind != "ok" || got.StatusCode != 200 {
		t.Fatalf("Check() = %+v, want response before body completes", got)
	}
}
