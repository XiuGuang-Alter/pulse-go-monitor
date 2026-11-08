package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Display values never become request/configuration values. Preserve the raw
// query order and encoding even here, so a signed URL remains recognizable.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "[无效网址]"
	}
	u.User = nil
	u.Fragment = ""
	u.RawFragment = ""
	parts := strings.Split(u.RawQuery, "&")
	for i, part := range parts {
		key, _, _ := strings.Cut(part, "=")
		decoded, err := url.QueryUnescape(key)
		if err != nil {
			parts[i] = "REDACTED"
			continue
		}
		switch strings.ToLower(decoded) {
		case "token", "access_token", "refresh_token", "api_key", "apikey", "secret", "password", "passwd", "signature", "sig":
			parts[i] = key + "=REDACTED"
		}
	}
	u.RawQuery = strings.Join(parts, "&")
	return u.String()
}

// Legacy free-text details can contain whole URLs or upstream content. Use
// stable explanations rather than attempting to recover their structure.
func safeDetail(kind string, status int, original ...string) string {
	if kind == "assertion_failed" && len(original) > 0 {
		allowed := []string{"文本规则：未包含指定内容", "JSON规则：正文格式无效或超过解析限制"}
		for i := 1; i <= 5; i++ {
			allowed = append(allowed, fmt.Sprintf("JSON规则%d：字段不存在或路径无效", i), fmt.Sprintf("JSON规则%d：值或类型不匹配", i))
		}
		for _, message := range allowed {
			if original[0] == message {
				return message
			}
		}
	}

	switch kind {
	case "ok":
		return "本次检查通过"
	case "http_error":
		return fmt.Sprintf("HTTP 异常状态：%d %s", status, http.StatusText(status))
	case "timeout":
		return "检查超时"
	case "request_error":
		return "网络请求或正文读取失败，请查看错误分类"
	case "assertion_failed":
		return "业务断言未通过，请核对规则与目标响应"
	case "body_too_large":
		return "正文超过1MiB读取上限"
	case "invalid_input":
		return "输入无效，请核对HTTP/HTTPS网址与规则"
	default:
		return "未知检查结果"
	}
}

func safeCheck(record CheckRecord) CheckRecord {
	record.InputURL = redactURL(record.InputURL)
	if record.FinalURL != "" {
		record.FinalURL = redactURL(record.FinalURL)
	}
	record.Detail = safeDetail(record.Kind, record.StatusCode, record.Detail)
	return record
}

type displayTarget struct {
	Target
	DisplayURL string `json:"display_url"`
}

func targetForDisplay(target Target) displayTarget {
	if target.Latest != nil {
		latest := safeCheck(*target.Latest)
		target.Latest = &latest
	}
	return displayTarget{Target: target, DisplayURL: redactURL(target.URL)}
}

func safeCategoryLabel(category string) string {
	labels := map[string]string{"dns": "DNS解析", "connection": "连接", "tls": "TLS", "timeout": "超时", "http_4xx": "HTTP 4xx", "http_5xx": "HTTP 5xx", "http_other": "其他HTTP", "body_read": "正文读取", "body_too_large": "正文超限", "assertion": "业务断言", "input": "输入", "unknown": "未知/旧分类"}
	if label, ok := labels[category]; ok {
		return label
	}
	return "未知"
}
