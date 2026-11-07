package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The server announces its actual address, so tests can use port 0 without
// releasing a reserved port and racing another listener.
type serveAnnouncement chan string

func (ready serveAnnouncement) Write(p []byte) (int, error) {
	if _, address, ok := strings.Cut(string(p), "http://"); ok {
		address, _, _ = strings.Cut(address, "\n")
		select {
		case ready <- "http://" + address:
		default:
		}
	}
	return len(p), nil
}

func TestServeShutdownCancelsCheckAndPreservesConfiguration(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	requestCanceled := make(chan struct{}, 1)
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestStarted <- struct{}{}
		select {
		case <-r.Context().Done():
			requestCanceled <- struct{}{}
		case <-unblock:
		}
	}))
	t.Cleanup(upstream.Close)
	t.Cleanup(func() { close(unblock) })
	path := filepath.Join(t.TempDir(), "monitor.db")
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(serveAnnouncement, 1)
	done := make(chan struct{})
	var errOut bytes.Buffer
	var exitCode int
	go func() {
		exitCode = runServe(ctx, []string{"-addr", "127.0.0.1:0", "-db", path}, ready, &errOut)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("service failed to stop")
		}
	})
	var baseURL string
	select {
	case baseURL = <-ready:
	case <-done:
		t.Fatalf("startup exit=%d: %s", exitCode, &errOut)
	case <-time.After(5 * time.Second):
		t.Fatal("service failed to announce startup")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(baseURL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Status string `json:"status"`
	}
	err = json.NewDecoder(response.Body).Decode(&status)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || status.Status != "ok" {
		t.Fatalf("status=%+v HTTP=%d err=%v", status, response.StatusCode, err)
	}
	input := TargetInput{Name: "in-flight shutdown", URL: upstream.URL, IntervalSeconds: 60, TimeoutSeconds: 30, Enabled: true}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response, err = client.Post(baseURL+"/api/targets", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatalf("create status=%d", response.StatusCode)
	}
	select {
	case <-requestStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("automatic check did not reach upstream")
	}
	cancel()
	select {
	case <-done:
		if exitCode != 0 || errOut.Len() != 0 {
			t.Fatalf("shutdown exit=%d: %s", exitCode, &errOut)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown waited for the 30-second target timeout")
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel upstream HTTP request")
	}
	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(baseURL, "http://"), time.Second)
	if err == nil {
		connection.Close()
		t.Fatal("listener still accepts connections after shutdown")
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	targets, err := store.ListTargets(context.Background())
	if err != nil || len(targets) != 1 {
		t.Fatalf("configuration not preserved: %+v %v", targets, err)
	}
	if !sameTargetInput(targets[0].TargetInput, input) || targets[0].Stats.Samples != 0 {
		t.Fatalf("configuration changed or shutdown recorded as target failure: %+v", targets[0])
	}
}

func TestServeStartupFailures(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"occupied port", []string{"-addr", occupied.Addr().String(), "-db", filepath.Join(t.TempDir(), "monitor.db")}},
		{"invalid database path", []string{"-addr", "127.0.0.1:0", "-db", filepath.Join(blocker, "monitor.db")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := runServe(context.Background(), tc.args, &out, &errOut); code != 1 || errOut.Len() == 0 || out.Len() != 0 {
				t.Fatalf("exit=%d out=%q err=%q", code, &out, &errOut)
			}
		})
	}
}

func TestDemoHTTPBehavior(t *testing.T) {
	server := httptest.NewServer(demoHandler())
	defer server.Close()
	for _, tc := range []struct {
		path   string
		status int
		kind   string
	}{{"/ok", 200, "ok"}, {"/error", 500, "http_error"}, {"/redirect", 200, "ok"}, {"/slow", 0, "timeout"}} {
		t.Run(tc.path, func(t *testing.T) {
			timeout := 2 * time.Second
			if tc.path == "/slow" {
				timeout = 100 * time.Millisecond
			}
			result := Check(context.Background(), server.URL+tc.path, timeout)
			if result.Kind != tc.kind || result.StatusCode != tc.status {
				t.Fatalf("%s: %+v, want %s HTTP%d", tc.path, result, tc.kind, tc.status)
			}
			if tc.path == "/redirect" && result.URL != server.URL+"/ok" {
				t.Fatalf("redirect ended at %s", result.URL)
			}
		})
	}
}
