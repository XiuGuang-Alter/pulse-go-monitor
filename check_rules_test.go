package main

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCheckRulesAndBodyLimit(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		rules      CheckRules
		want       string
		gzip       bool
	}{
		{"business failure", `{"code":1}`, 200, CheckRules{JSONEquals: []JSONAssertion{{Pointer: "/code", Equals: []byte(`0`)}}}, "assertion_failed", false},
		{"custom status", "", 404, CheckRules{ExpectedStatuses: []int{404}}, "ok", false},
		{"at limit", "ok" + strings.Repeat("x", (1<<20)-2), 200, CheckRules{BodyContains: "ok"}, "ok", false},
		{"over limit", "ok" + strings.Repeat("x", (1<<20)-1), 200, CheckRules{BodyContains: "ok"}, "body_too_large", false},
		{"compressed limit", strings.Repeat("x", (1<<20)+1), 200, CheckRules{BodyContains: "x"}, "body_too_large", true},
		{"private body", "PRIVATE_VALUE", 200, CheckRules{BodyContains: "not found"}, "assertion_failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.gzip {
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.WriteHeader(tc.status)
				if tc.gzip {
					gz := gzip.NewWriter(w)
					gz.Write([]byte(tc.body))
					gz.Close()
				} else {
					w.Write([]byte(tc.body))
				}
			}))
			defer s.Close()
			got := CheckWithRules(context.Background(), s.URL, time.Second, tc.rules)
			if got.Kind != tc.want {
				t.Fatalf("got%+v want%s", got, tc.want)
			}
			if strings.Contains(got.Detail, "PRIVATE_VALUE") {
				t.Fatal("response body leaked")
			}
			if got.TotalDuration < got.Duration {
				t.Fatal("invalid timings")
			}
		})
	}
}
func TestCheckBodyTimeoutCancelAndTruncation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/truncated" {
			w.Header().Set("Content-Length", "100")
			w.Write([]byte("short"))
			return
		}
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	rules := CheckRules{BodyContains: "ok"}
	got := CheckWithRules(context.Background(), s.URL, 50*time.Millisecond, rules)
	if got.Kind != "timeout" || got.StatusCode != 200 {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() { done <- CheckWithRules(ctx, s.URL, time.Second, rules) }()
	time.AfterFunc(30*time.Millisecond, cancel)
	select {
	case got = <-done:
		if got.Kind != "request_error" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("body cancellation hung")
	}
	got = CheckWithRules(context.Background(), s.URL+"/truncated", time.Second, rules)
	if got.Kind != "request_error" {
		t.Fatal("truncation misclassified", got)
	}
}
