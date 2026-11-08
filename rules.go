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
	invalid := func(reason string) (CheckRules, error) {
		return CheckRules{}, fmt.Errorf("%w：%s", ErrValidation, reason)
	}
	invalidRule := func(index int, reason string) (CheckRules, error) {
		return invalid(fmt.Sprintf("JSON规则%d：%s", index+1, reason))
	}
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return rules, nil
	}
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return invalid("检查规则：需要有效的UTF-8 JSON，控制字符须显式转义")
	}
	// Decode fields separately so errors identify only trusted field names and
	// rule numbers. Never return decoder errors, which can contain user input.
	var fields struct {
		ExpectedStatuses json.RawMessage `json:"expected_statuses"`
		BodyContains     json.RawMessage `json:"body_contains"`
		JSONEquals       json.RawMessage `json:"json_equals"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fields); err != nil {
		return invalid("检查规则：需要JSON对象，仅支持expected_statuses、body_contains和json_equals，不允许未知字段")
	}
	if len(fields.ExpectedStatuses) > 0 {
		if err := json.Unmarshal(fields.ExpectedStatuses, &rules.ExpectedStatuses); err != nil {
			return invalid("expected_statuses：需要200–599的整数数组，最多10项")
		}
	}
	if len(rules.ExpectedStatuses) > 10 {
		return invalid("expected_statuses：最多10项状态码")
	}
	if len(fields.BodyContains) > 0 {
		if err := json.Unmarshal(fields.BodyContains, &rules.BodyContains); err != nil {
			return invalid("body_contains：需要字符串，最长256个UTF-8字节")
		}
	}
	if len(rules.BodyContains) > 256 {
		return invalid("body_contains：最长256个UTF-8字节，按字节而非字符计数")
	}
	var assertions []json.RawMessage
	if len(fields.JSONEquals) > 0 {
		if err := json.Unmarshal(fields.JSONEquals, &assertions); err != nil {
			return invalid("json_equals：需要JSON规则数组，最多5项")
		}
	}
	if len(assertions) > 5 {
		return invalid("json_equals：最多5项JSON规则")
	}
	codes := map[int]bool{}
	for _, code := range rules.ExpectedStatuses {
		if code < 200 || code > 599 {
			return invalid("expected_statuses：状态码范围为200–599")
		}
		codes[code] = true
	}
	rules.ExpectedStatuses = nil
	for code := range codes {
		rules.ExpectedStatuses = append(rules.ExpectedStatuses, code)
	}
	sort.Ints(rules.ExpectedStatuses)
	for index, rawAssertion := range assertions {
		var assertion JSONAssertion
		decoder := json.NewDecoder(bytes.NewReader(rawAssertion))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&assertion); err != nil {
			var typeError *json.UnmarshalTypeError
			if errors.As(err, &typeError) && typeError.Field == "pointer" {
				return invalidRule(index, "pointer：需要JSON Pointer字符串，最长256个UTF-8字节")
			}
			return invalidRule(index, "需要规则对象，仅支持pointer和equals，不允许未知字段；pointer须为字符串")
		}
		if len(assertion.Pointer) > 256 {
			return invalidRule(index, "pointer：最长256个UTF-8字节，按字节而非字符计数")
		}
		if _, err := pointerTokens(assertion.Pointer); err != nil {
			return invalidRule(index, "pointer：需要有效JSON Pointer，留空表示根值，否则以/开头；~须转义为~0，/须转义为~1")
		}
		if len(assertion.Equals) == 0 {
			return invalidRule(index, "equals：必须填写JSON标量（字符串、数字、布尔值或null），不支持对象或数组")
		}
		expected, e := decodeStrictJSON(context.Background(), assertion.Equals)
		if e != nil {
			return invalidRule(index, "equals：需要有效JSON标量；数字最长256字节，指数范围为-1000至1000")
		}
		switch v := expected.(type) {
		case nil, bool, json.Number:
		case string:
			if len(v) > 256 {
				return invalidRule(index, "equals：字符串最长256个UTF-8字节，按字节而非字符计数")
			}
		default:
			return invalidRule(index, "equals：仅支持JSON标量（字符串、数字、布尔值或null），不支持对象或数组")
		}
		if _, err := decodeStrictJSON(context.Background(), rawAssertion); err != nil {
			return invalidRule(index, "JSON格式无效、含重复字段或超过解析限制")
		}
		rules.JSONEquals = append(rules.JSONEquals, assertion)
	}
	if _, err := decodeStrictJSON(context.Background(), raw); err != nil {
		return invalid("检查规则：JSON格式无效、含重复字段或超过解析限制")
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
