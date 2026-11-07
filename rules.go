package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

type CheckRules struct {
	ExpectedStatuses []int           `json:"expected_statuses,omitempty"`
	BodyContains     string          `json:"body_contains,omitempty"`
	JSONEquals       []JSONAssertion `json:"json_equals,omitempty"`
}
type JSONAssertion struct {
	Pointer string          `json:"pointer"`
	Equals  json.RawMessage `json:"equals"`
}

func parseRules(raw json.RawMessage) (CheckRules, error) {
	var rules CheckRules
	invalid := func() (CheckRules, error) {
		return CheckRules{}, fmt.Errorf("%w：检查规则无效或超过限制", ErrValidation)
	}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return rules, nil
	}
	value, err := decodeStrictJSON(context.Background(), raw)
	if err != nil {
		return invalid()
	}
	if _, ok := value.(map[string]any); !ok {
		return invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&rules); err != nil {
		return invalid()
	}
	if len(rules.ExpectedStatuses) > 10 || len(rules.BodyContains) > 256 || len(rules.JSONEquals) > 5 {
		return invalid()
	}
	codes := map[int]bool{}
	for _, code := range rules.ExpectedStatuses {
		if code < 200 || code > 599 {
			return invalid()
		}
		codes[code] = true
	}
	rules.ExpectedStatuses = nil
	for code := range codes {
		rules.ExpectedStatuses = append(rules.ExpectedStatuses, code)
	}
	sort.Ints(rules.ExpectedStatuses)
	for _, assertion := range rules.JSONEquals {
		if _, err = pointerTokens(assertion.Pointer); err != nil || len(assertion.Equals) == 0 {
			return invalid()
		}
		expected, e := decodeStrictJSON(context.Background(), assertion.Equals)
		if e != nil {
			return invalid()
		}
		switch v := expected.(type) {
		case nil, bool, json.Number:
		case string:
			if len(v) > 256 {
				return invalid()
			}
		default:
			return invalid()
		}
	}
	return rules, nil
}
func (r CheckRules) hasBody() bool { return r.BodyContains != "" || len(r.JSONEquals) > 0 }
func (r CheckRules) acceptsStatus(code int) bool {
	if len(r.ExpectedStatuses) == 0 {
		return code >= 200 && code <= 299
	}
	for _, v := range r.ExpectedStatuses {
		if code == v {
			return true
		}
	}
	return false
}
func evaluateBody(ctx context.Context, body []byte, rules CheckRules) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rules.BodyContains != "" && !bytes.Contains(body, []byte(rules.BodyContains)) {
		return errors.New("文本规则：未包含指定内容")
	}
	if len(rules.JSONEquals) == 0 {
		return ctx.Err()
	}
	value, err := decodeStrictJSON(ctx, body)
	if err != nil {
		return errors.New("JSON规则：正文格式无效或超过解析限制")
	}
	for i, rule := range rules.JSONEquals {
		if err = ctx.Err(); err != nil {
			return err
		}
		actual, e := resolvePointer(value, rule.Pointer)
		if e != nil {
			return fmt.Errorf("JSON规则%d：字段不存在或路径无效", i+1)
		}
		expected, e := decodeStrictJSON(ctx, rule.Equals)
		if e != nil || !scalarEqual(actual, expected) {
			return fmt.Errorf("JSON规则%d：值或类型不匹配", i+1)
		}
	}
	return ctx.Err()
}
func pointerTokens(pointer string) ([]string, error) {
	if len(pointer) > 256 || !utf8.ValidString(pointer) {
		return nil, ErrValidation
	}
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, ErrValidation
	}
	parts := strings.Split(pointer[1:], "/")
	for n, part := range parts {
		var b strings.Builder
		for i := 0; i < len(part); i++ {
			if part[i] != '~' {
				b.WriteByte(part[i])
				continue
			}
			i++
			if i >= len(part) || (part[i] != '0' && part[i] != '1') {
				return nil, ErrValidation
			}
			if part[i] == '0' {
				b.WriteByte('~')
			} else {
				b.WriteByte('/')
			}
		}
		parts[n] = b.String()
	}
	return parts, nil
}
func resolvePointer(value any, pointer string) (any, error) {
	tokens, err := pointerTokens(pointer)
	if err != nil {
		return nil, err
	}
	for _, token := range tokens {
		switch v := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = v[token]
			if !ok {
				return nil, ErrNotFound
			}
		case []any:
			if token == "" || (len(token) > 1 && token[0] == '0') {
				return nil, ErrValidation
			}
			for _, c := range token {
				if c < '0' || c > '9' {
					return nil, ErrValidation
				}
			}
			idx, e := strconv.Atoi(token)
			if e != nil || idx < 0 || idx >= len(v) {
				return nil, ErrNotFound
			}
			value = v[idx]
		default:
			return nil, ErrNotFound
		}
	}
	return value, nil
}
func decodeStrictJSON(ctx context.Context, data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, ErrValidation
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readJSON(ctx, decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, ErrValidation
	}
	return value, ctx.Err()
}
