package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRedactURLQueryKeys(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://example.com/a?Token=fake-secret&x=1&%61pi_key=second&token=third#private", "https://example.com/a?Token=REDACTED&x=1&%61pi_key=REDACTED&token=REDACTED"},
		{"https://example.com/?q=a%20b&sig=x+y&empty=", "https://example.com/?q=a%20b&sig=REDACTED&empty="},
		{"https://example.com/#hidden", "https://example.com/"},
		{"https://example.com/?%token=bad", "https://example.com/?REDACTED"},
		{"https://example.com/%xx?token=fake-secret", "[无效网址]"},
	} {
		if got := redactURL(tc.in); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}

func TestSafeCheckKeepsOnlyKnownAssertionExplanation(t *testing.T) {
	known := safeCheck(CheckRecord{Kind: "assertion_failed", Detail: "JSON规则2：值或类型不匹配"})
	if known.Detail != "JSON规则2：值或类型不匹配" {
		t.Fatalf("lost safe rule location: %s", known.Detail)
	}
	hostile := safeCheck(CheckRecord{Kind: "assertion_failed", Detail: "JSON规则2：值或类型不匹配 token=fake-secret"})
	if strings.Contains(hostile.Detail, "fake-secret") {
		t.Fatal("untrusted suffix leaked")
	}
}

func TestDisplayDoesNotChangeRequestOrStoredConfiguration(t *testing.T) {
	seen := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen <- r.RequestURI; w.WriteHeader(500) }))
	defer upstream.Close()
	raw := upstream.URL + "/?sig=fake-signature&x=a%20b&token=fake-token"
	store := testStore(t)
	target, err := store.SaveTarget(context.Background(), 0, inputFor(raw))
	if err != nil {
		t.Fatal(err)
	}
	result := Check(context.Background(), raw, time.Second)
	if got := <-seen; got != "/?sig=fake-signature&x=a%20b&token=fake-token" {
		t.Fatalf("request changed: %s", got)
	}
	result.Detail = "old free text: " + raw
	if err = store.RecordCheck(context.Background(), target, result, time.Now()); err != nil {
		t.Fatal(err)
	}
	s, _ := NewScheduler(store, 1, nil)
	defer s.Stop()
	handler := NewApp(store, s).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", "http://127.0.0.1"+path, nil))
		return rr
	}
	rr := get("/api/targets")
	var targets []struct {
		URL        string `json:"url"`
		DisplayURL string `json:"display_url"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &targets); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].URL != raw || strings.Contains(targets[0].DisplayURL, "fake-") || !strings.Contains(targets[0].DisplayURL, "REDACTED") {
		t.Fatalf("bad target display: %s", rr.Body.String())
	}
	rr = get("/api/targets/1/checks")
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "fake-") {
		t.Fatalf("history leaked: %s", rr.Body.String())
	}
	var history struct {
		Categories map[string]int `json:"error_categories"`
		Scope      string         `json:"category_scope"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if history.Categories["http_5xx"] != 1 || history.Scope != "retained_checks" {
		t.Fatalf("missing retained classification: %s", rr.Body.String())
	}
	saved, _ := store.GetTarget(context.Background(), target.ID)
	if saved.URL != raw {
		t.Fatal("display altered source config")
	}
	var out, errOut bytes.Buffer
	if code := run([]string{raw}, &out, &errOut); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(out.String()+errOut.String(), "fake-") {
		t.Fatal("CLI leaked query values")
	}
}

func TestRedactsEveryProtectedKeyAndPreservesNonSecretEncoding(t *testing.T) {
	raw := "https://example.com/?token=a&access_token=b&refresh_token=c&API_KEY=d&apikey=e&secret=f&password=g&passwd=h&signature=i&sig=j&visible=a%2Bb%20c"
	got := redactURL(raw)
	want := "https://example.com/?token=REDACTED&access_token=REDACTED&refresh_token=REDACTED&API_KEY=REDACTED&apikey=REDACTED&secret=REDACTED&password=REDACTED&passwd=REDACTED&signature=REDACTED&sig=REDACTED&visible=a%2Bb%20c"
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestRedirectFinalURLAndNetworkErrorsAreSafe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/finish?secret=fake-redirect", 302)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	result := Check(context.Background(), server.URL+"/start?token=fake-input", time.Second)
	record := safeCheck(CheckRecord{Kind: result.Kind, InputURL: server.URL + "/start?token=fake-input", FinalURL: result.URL, Detail: result.Detail})
	if strings.Contains(record.InputURL+record.FinalURL+record.Detail, "fake-") || !strings.Contains(record.FinalURL, "REDACTED") {
		t.Fatalf("unsafe redirect: %+v", record)
	}
	server.Close()
	failed := Check(context.Background(), server.URL+"/?password=fake-network", time.Second)
	if failed.Kind == "ok" || strings.Contains(failed.Detail, "fake-network") || strings.Contains(failed.Detail, server.URL) {
		t.Fatalf("unsafe network detail: %+v", failed)
	}
}
