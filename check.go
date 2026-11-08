package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// Result 描述一次检查。Duration 到最终响应头为止，不包含正文下载。
type Result struct {
	URL              string
	StatusCode       int
	Duration         time.Duration
	Kind             string
	ErrorCategory    string
	Detail           string
	TotalDuration    time.Duration
	durationMeasured bool
}

// Check 只检查一次；timeout 覆盖连接、重定向和等待响应头。
func Check(ctx context.Context, rawURL string, timeout time.Duration) Result {
	return CheckWithRules(ctx, rawURL, timeout, CheckRules{})
}
func CheckWithRules(ctx context.Context, rawURL string, timeout time.Duration, rules CheckRules) (result Result) {
	result = Result{URL: rawURL}
	defer func() {
		result.ErrorCategory = normalizedErrorCategory(result.Kind, result.StatusCode, result.ErrorCategory)
	}()
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		result.Kind = "invalid_input"
		result.Detail = "请输入包含主机名的完整 http:// 或 https:// 网址，且不要包含用户名或密码"
		return result
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			result.Kind, result.Detail = "invalid_input", "端口必须是 1 到 65535 之间的整数"
			return result
		}
	}

	// defer 确保函数从任意分支返回时，释放定时器资源。
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		result.Kind, result.Detail = "invalid_input", "无法构建HTTP请求，请检查网址"
		return result
	}

	client := &http.Client{}
	var tlsFailed atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				tlsFailed.Store(true)
			}
		},
	}))
	started := time.Now()
	defer func() { result.TotalDuration = time.Since(started); result.durationMeasured = true }()
	response, err := client.Do(req)
	result.Duration = time.Since(started)
	if err != nil {
		result.ErrorCategory = requestErrorCategory(err)
		if result.ErrorCategory != "timeout" && tlsFailed.Load() {
			result.ErrorCategory = "tls"
		}
		result.Kind, result.Detail = "request_error", requestErrorDetail(result.ErrorCategory)
		if errors.Is(err, context.DeadlineExceeded) {
			result.Kind = "timeout"
		}
		return result
	}
	defer response.Body.Close()
	result.URL = response.Request.URL.String()
	result.StatusCode = response.StatusCode
	if rules.acceptsStatus(response.StatusCode) {
		result.Kind, result.Detail = "ok", "本次 HTTP 检查通过"
	} else {
		result.Kind = "http_error"
		result.Detail = fmt.Sprintf("HTTP 异常状态：%d %s", response.StatusCode, http.StatusText(response.StatusCode))
		return result
	}
	if !rules.hasBody() {
		return result
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		result.Kind, result.Detail = "request_error", "正文读取失败"
		result.ErrorCategory = "body_read"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Kind, result.Detail = "timeout", "正文读取超时"
			result.ErrorCategory = "timeout"
		}
		return result
	}
	if len(body) > 1<<20 {
		result.Kind, result.Detail = "body_too_large", "正文超过1MiB读取上限"
		return result
	}
	err = evaluateBody(ctx, body, rules)
	if ctx.Err() != nil {
		result.Kind, result.Detail = "request_error", "检查已取消"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Kind, result.Detail = "timeout", "检查超时"
		}
		return result
	}
	if err != nil {
		result.Kind, result.Detail = "assertion_failed", err.Error()
	} else {
		result.Detail = "HTTP与业务断言通过"
	}
	return result
}

// requestErrorCategory inspects wrapped error types; it never guesses from text.
func requestErrorCategory(err error) string {
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var verification *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certificate x509.CertificateInvalidError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	if errors.As(err, &verification) || errors.As(err, &authority) || errors.As(err, &hostname) || errors.As(err, &certificate) || errors.As(err, &record) || errors.As(err, &alert) {
		return "tls"
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		return "connection"
	}
	return "unknown"
}

func requestErrorDetail(category string) string {
	switch category {
	case "dns":
		return "域名解析失败，请检查域名和DNS设置"
	case "connection":
		return "网络连接失败，请检查目标服务和网络连接"
	case "tls":
		return "TLS安全连接失败，请检查证书和HTTPS配置"
	case "timeout":
		return "检查超时，请检查目标响应和超时设置"
	default:
		return "网络请求失败，请检查目标网址和网络状态"
	}
}

// Missing legacy categories are inferred only when kind/status establish the fact.
func normalizedErrorCategory(kind string, status int, category string) string {
	if kind == "ok" {
		return ""
	}
	if category != "" {
		return category
	}
	switch kind {
	case "invalid_input":
		return "input"
	case "timeout":
		return "timeout"
	case "body_too_large":
		return "body_too_large"
	case "assertion_failed":
		return "assertion"
	case "http_error":
		if status >= 400 && status <= 499 {
			return "http_4xx"
		}
		if status >= 500 && status <= 599 {
			return "http_5xx"
		}
		return "http_other"
	default:
		return "unknown"
	}
}
