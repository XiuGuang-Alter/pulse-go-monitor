package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRuleBudgetsAtAndBeyondBoundary(t *testing.T) {
	statuses := func(n int) CheckRules {
		r := CheckRules{}
		for i := 0; i < n; i++ {
			r.ExpectedStatuses = append(r.ExpectedStatuses, 200+i)
		}
		return r
	}
	assertions := func(n int) CheckRules {
		r := CheckRules{}
		for i := 0; i < n; i++ {
			r.JSONEquals = append(r.JSONEquals, JSONAssertion{Pointer: "/x", Equals: json.RawMessage(`0`)})
		}
		return r
	}
	scalar := func(pointer, value string) CheckRules {
		return CheckRules{JSONEquals: []JSONAssertion{{Pointer: pointer, Equals: json.RawMessage(value)}}}
	}
	cases := []struct {
		name  string
		rules CheckRules
		valid bool
	}{
		{"ten statuses", statuses(10), true}, {"eleven statuses", statuses(11), false},
		{"five assertions", assertions(5), true}, {"six assertions", assertions(6), false},
		{"256 UTF8 bytes", CheckRules{BodyContains: strings.Repeat("é", 128)}, true},
		{"258 UTF8 bytes", CheckRules{BodyContains: strings.Repeat("é", 129)}, false},
		{"pointer256", scalar("/"+strings.Repeat("a", 255), `0`), true},
		{"pointer257", scalar("/"+strings.Repeat("a", 256), `0`), false},
		{"string256", scalar("", mustJSON(strings.Repeat("a", 256))), true},
		{"string257", scalar("", mustJSON(strings.Repeat("a", 257))), false},
		{"number256", scalar("", strings.Repeat("1", 256)), true},
		{"number257", scalar("", strings.Repeat("1", 257)), false},
		{"positive exponent1000", scalar("", `1e1000`), true},
		{"negative exponent1000", scalar("", `1e-1000`), true},
		{"negative exponent1001", scalar("", `1e-1001`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.rules)
			if err != nil {
				t.Fatal(err)
			}
			_, err = parseRules(raw)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
	for _, depth := range []int{64, 65} {
		body := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
		rules := scalar(strings.Repeat("/0", depth), `0`)
		err := evaluateBody(context.Background(), []byte(body), rules)
		if (err == nil) != (depth == 64) {
			t.Fatalf("depth%d: %v", depth, err)
		}
	}
	for _, body := range [][]byte{[]byte(`{"a":1,"a":1}`), {'"', 0xff, '"'}} {
		if _, err := decodeStrictJSON(context.Background(), body); err == nil {
			t.Fatal("ambiguous or invalid UTF8 body accepted")
		}
	}
}
