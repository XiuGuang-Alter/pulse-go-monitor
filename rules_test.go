package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRulesValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"expected_statuses":[200,404],"body_contains":"ok","json_equals":[{"pointer":"/code","equals":0}]}`} {
		if _, err := parseRules(json.RawMessage(raw)); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{`[]`, `{"other":1}`, `{"expected_statuses":[199]}`, `{"expected_statuses":[600]}`, `{"json_equals":[{"pointer":"bad","equals":0}]}`, `{"json_equals":[{"pointer":"/~2","equals":0}]}`, `{"json_equals":[{"pointer":"/a"}]}`, `{"json_equals":[{"pointer":"/a","equals":{}}]}`, `{"json_equals":[{"pointer":"/a","equals":1e1001}]}`, `{"body_contains":"` + strings.Repeat("a", 257) + `"}`} {
		if _, err := parseRules(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
func TestRuleBodyEvaluation(t *testing.T) {
	cases := []struct {
		body, pointer, value string
		ok                   bool
	}{
		{`{"code":0}`, "/code", `0`, true}, {`{"code":1}`, "/code", `0`, false},
		{`{"code":null}`, "/code", `null`, true}, {`{}`, "/code", `null`, false},
		{`{"a/b":{"~":[false]}}`, "/a~1b/~0/0", `false`, true},
		{`[1]`, "/01", `1`, false}, {`{"n":9007199254740993}`, "/n", `9007199254740992`, false},
		{`{"n":1.0}`, "/n", `1`, true}, {`1000`, "", `1e3`, true},
		{`{"n":1,"n":2}`, "/n", `2`, false}, {`{"n":"1"}`, "/n", `1`, false},
		{strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65), "/0", `0`, false},
		{`{"n":1e1001}`, "/n", `1`, false}, {`{"n":1} {}`, "/n", `1`, false},
	}
	for _, tc := range cases {
		rules, err := parseRules(json.RawMessage(`{"json_equals":[{"pointer":` + mustJSON(tc.pointer) + `,"equals":` + tc.value + `}]} `))
		if err != nil {
			t.Fatal(err)
		}
		err = evaluateBody(context.Background(), []byte(tc.body), rules)
		if (err == nil) != tc.ok {
			t.Errorf("body=%s path=%s got%v", tc.body, tc.pointer, err)
		}
	}
	r := CheckRules{BodyContains: "secret-not-here"}
	err := evaluateBody(context.Background(), []byte("PRIVATE_RESPONSE"), r)
	if err == nil || strings.Contains(err.Error(), "PRIVATE_RESPONSE") {
		t.Fatal("body leaked or rule passed", err)
	}
}
func mustJSON(value any) string { raw, _ := json.Marshal(value); return string(raw) }
