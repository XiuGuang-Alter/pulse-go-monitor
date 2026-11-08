package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRulesValidationLocatesSafeActionableErrors(t *testing.T) {
	const secret = "SECRET_TOKEN_PRIVATE_BODY"
	cases := []struct {
		name, raw string
		want      []string
	}{
		{"status type", `{"expected_statuses":"` + secret + `"}`, []string{"expected_statuses", "整数"}},
		{"status range", `{"expected_statuses":[200,199]}`, []string{"expected_statuses", "200", "599"}},
		{"status count", `{"expected_statuses":[200,201,202,203,204,205,206,207,208,209,210]}`, []string{"expected_statuses", "10"}},
		{"body type", `{"body_contains":{"secret":"` + secret + `"}}`, []string{"body_contains", "字符串"}},
		{"body bytes", `{"body_contains":` + mustJSON(strings.Repeat("中", 86)+secret) + `}`, []string{"body_contains", "UTF-8", "256", "字节"}},
		{"assertions type", `{"json_equals":"` + secret + `"}`, []string{"json_equals", "数组"}},
		{"assertions count", `{"json_equals":[{},{},{},{},{},{}]}`, []string{"json_equals", "5"}},
		{"second pointer", `{"json_equals":[{"pointer":"","equals":1},{"pointer":"` + secret + `","equals":0}]}`, []string{"JSON规则2", "pointer", "JSON Pointer"}},
		{"second pointer type", `{"json_equals":[{"equals":1},{"pointer":123,"equals":0}]}`, []string{"JSON规则2", "pointer", "字符串"}},
		{"pointer bytes", `{"json_equals":[{"pointer":` + mustJSON("/"+strings.Repeat("中", 86)+secret) + `,"equals":0}]}`, []string{"JSON规则1", "pointer", "UTF-8", "256", "字节"}},
		{"missing equals", `{"json_equals":[{"equals":0},{"pointer":"/` + secret + `"}]}`, []string{"JSON规则2", "equals", "标量"}},
		{"object equals", `{"json_equals":[{"equals":0},{"equals":{"secret":"` + secret + `"}}]}`, []string{"JSON规则2", "equals", "标量", "对象", "数组"}},
		{"array equals", `{"json_equals":[{"equals":[]}]}`, []string{"JSON规则1", "equals", "标量"}},
		{"string bytes", `{"json_equals":[{"equals":` + mustJSON(strings.Repeat("中", 86)+secret) + `}]}`, []string{"JSON规则1", "equals", "UTF-8", "256", "字节"}},
		{"number budget", `{"json_equals":[{"equals":0},{"equals":1e1001}]}`, []string{"JSON规则2", "equals", "256", "1000"}},
		{"unknown top field", `{"` + secret + `":1}`, []string{"检查规则", "未知字段"}},
		{"unknown assertion field", `{"json_equals":[{"equals":0},{"equals":1,"` + secret + `":2}]}`, []string{"JSON规则2", "未知字段"}},
		{"duplicate assertion field", `{"json_equals":[{"equals":0},{"equals":"` + secret + `","equals":1}]}`, []string{"JSON规则2", "重复字段"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRules(json.RawMessage(tc.raw))
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
			for _, part := range tc.want {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error lacks safe location or correction %q: %v", part, err)
				}
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("validation error disclosed input: %v", err)
			}
		})
	}
}

func TestRulesValidationPreservesValidPayloads(t *testing.T) {
	raw := json.RawMessage(`{"EXPECTED_STATUSES":[404,200,404],"BODY_CONTAINS":"line1\nline2\r\n\t\u0000","JSON_EQUALS":[{"POINTER":"/n","EQUALS":9007199254740993},{"pointer":"","equals":null}]}`)
	rules, err := parseRules(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(rules.JSONEquals[0].Equals) != "9007199254740993" || rules.BodyContains != "line1\nline2\r\n\t\x00" || len(rules.ExpectedStatuses) != 2 || rules.ExpectedStatuses[0] != 200 || rules.ExpectedStatuses[1] != 404 {
		t.Fatalf("valid rule values changed: %#v", rules)
	}
	for _, raw := range []string{`{"body_contains":"a","body_contains":"b"}`, `{"json_equals":[{"equals":"line
break"}]}`} {
		if _, err := parseRules(json.RawMessage(raw)); !errors.Is(err, ErrValidation) {
			t.Fatalf("strict JSON validation lost: %v", err)
		}
	}
}
