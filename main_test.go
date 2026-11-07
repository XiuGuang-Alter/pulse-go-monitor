package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/error" {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	for _, tc := range []struct {
		name string
		args []string
		code int
		text []string
	}{
		{"ok", []string{server.URL}, 0, []string{"200", "耗时", "通过"}},
		{"http_error", []string{server.URL + "/error"}, 1, []string{"500", "HTTP 异常状态", "耗时"}},
		{"no_args", nil, 2, []string{"用法"}},
		{"extra_args", []string{server.URL, server.URL}, 2, []string{"用法"}},
		{"invalid", []string{"example.com"}, 2, []string{"输入无效"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := run(tc.args, &out, &errOut)
			if code != tc.code {
				t.Fatalf("exit = %d, want %d; out=%s err=%s", code, tc.code, &out, &errOut)
			}
			text := out.String() + errOut.String()
			for _, fragment := range tc.text {
				if !strings.Contains(text, fragment) {
					t.Errorf("output %q missing %q", text, fragment)
				}
			}
			if tc.code == 0 && errOut.Len() != 0 {
				t.Errorf("successful check wrote stderr: %s", &errOut)
			}
		})
	}
}

// 验证用户实际等待时间，防止默认值未被命令行使用。
func TestRunDefaultTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	var out, errOut bytes.Buffer
	start := time.Now()
	code := run([]string{server.URL}, &out, &errOut)
	elapsed := time.Since(start)
	if code != 1 || !strings.Contains(errOut.String(), "超时") || elapsed < 4500*time.Millisecond || elapsed > 8*time.Second {
		t.Fatalf("exit=%d elapsed=%s out=%s err=%s, want timeout around 5s", code, elapsed, &out, &errOut)
	}
}
