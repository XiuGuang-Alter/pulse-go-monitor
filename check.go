package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Result 描述一次检查。Duration 到最终响应头为止，不包含正文下载。
type Result struct {
	URL           string
	StatusCode    int
	Duration      time.Duration
	Kind          string
	Detail        string
	TotalDuration time.Duration
}

// Check 只检查一次；timeout 覆盖连接、重定向和等待响应头。
func Check(ctx context.Context, rawURL string, timeout time.Duration) Result {
	return CheckWithRules(ctx, rawURL, timeout, CheckRules{})
}
func CheckWithRules(ctx context.Context, rawURL string, timeout time.Duration, rules CheckRules) (result Result) {
	result = Result{URL: rawURL}
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
		result.Kind, result.Detail = "invalid_input", "无法构建请求："+err.Error()
		return result
	}

	client := &http.Client{}
	started := time.Now()
	defer func() { result.TotalDuration = time.Since(started) }()
	response, err := client.Do(req)
	result.Duration = time.Since(started)
	if err != nil {
		result.Kind, result.Detail = "request_error", "网络请求失败："+err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			result.Kind, result.Detail = "timeout", "检查超时："+err.Error()
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
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Kind, result.Detail = "timeout", "正文读取超时"
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
