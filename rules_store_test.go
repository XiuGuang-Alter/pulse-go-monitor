package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sameTargetInput(a, b TargetInput) bool {
	ar, e := parseRules(a.Rules)
	br, f := parseRules(b.Rules)
	if e != nil || f != nil {
		return false
	}
	a.Rules, _ = json.Marshal(ar)
	b.Rules, _ = json.Marshal(br)
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestRulesPersistenceAndOldPUT(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	in := inputFor("http://example.com")
	in.Rules = json.RawMessage(`{"body_contains":"ok"}`)
	target, err := s.SaveTarget(ctx, 0, in)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTarget(ctx, target.ID)
	if !strings.Contains(string(got.Rules), "body_contains") {
		t.Fatal("rules not saved", got)
	}
	in.Rules = nil
	in.Name = "old client update"
	got, err = s.SaveTarget(ctx, target.ID, in)
	if err != nil || !strings.Contains(string(got.Rules), "body_contains") {
		t.Fatal("old client cleared rules", got, err)
	}
	in.Rules = []byte(`null`)
	got, err = s.SaveTarget(ctx, target.ID, in)
	if err != nil || string(got.Rules) != "{}" {
		t.Fatal("null did not reset", got, err)
	}
	in.Rules = []byte(`{"json_equals":[{"pointer":"/a"}]}`)
	if _, err = s.SaveTarget(ctx, target.ID, in); err == nil {
		t.Fatal("invalid rule saved")
	}
	scheduler, _ := NewScheduler(s, 1, nil)
	defer scheduler.Stop()
	handler := NewApp(s, scheduler).Handler()
	in.Rules = nil
	raw, _ := json.Marshal(in)
	raw = append(raw, []byte(strings.Repeat(" ", 9000))...)
	request := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/targets", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != 201 {
		t.Fatal("16KiB request compatibility", w.Code, w.Body.String())
	}
}
func TestRulesImmutableInFlightSnapshot(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; w.Write([]byte(`{"code":1}`)) }))
	defer upstream.Close()
	s := testStore(t)
	in := inputFor(upstream.URL)
	in.Enabled = false
	in.Rules = []byte(`{"json_equals":[{"pointer":"/code","equals":0}]}`)
	target, err := s.SaveTarget(context.Background(), 0, in)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, _ := NewScheduler(s, 1, nil)
	defer scheduler.Stop()
	if err = scheduler.Trigger(target.ID); err != nil {
		t.Fatal(err)
	}
	<-started
	in.Rules = []byte(`{"json_equals":[{"pointer":"/code","equals":1}]}`)
	updated, err := s.SaveTarget(context.Background(), target.ID, in)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { rows, _ := s.History(context.Background(), target.ID, 10, 0); return len(rows) == 1 })
	rows, _ := s.History(context.Background(), target.ID, 10, 0)
	r := rows[0]
	if r.Kind != "assertion_failed" || r.ConfigVersion == nil || *r.ConfigVersion != target.Version || r.TotalDurationMS == nil || !strings.Contains(string(r.RulesSnapshot), `"equals":0`) {
		t.Fatal("snapshot/timing lost", r)
	}
	current, _ := s.GetTarget(context.Background(), target.ID)
	if current.Version != updated.Version || current.NextCheckAt != updated.NextCheckAt {
		t.Fatal("old task modified new schedule")
	}
	list, _ := s.ListTargets(context.Background())
	if list[0].Latest.ConfigVersion == nil {
		t.Fatal("list latest omitted snapshot")
	}
}
