package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	ErrNotFound   = errors.New("目标不存在")
	ErrValidation = errors.New("输入无效")
	ErrLimit      = errors.New("最多支持100个目标")
	ErrBusy       = errors.New("目标正在检查，或并发槽位已满，请稍后重试")
	ErrStopped    = errors.New("调度器已停止")
)

type TargetInput struct {
	Name            string          `json:"name"`
	URL             string          `json:"url"`
	IntervalSeconds int             `json:"interval_seconds"`
	TimeoutSeconds  int             `json:"timeout_seconds"`
	Enabled         bool            `json:"enabled"`
	Rules           json.RawMessage `json:"rules,omitempty"`
}
type SampleStats struct {
	Samples      int     `json:"samples"`
	Successes    int     `json:"successes"`
	Availability float64 `json:"availability"`
	AverageMS    float64 `json:"average_ms"`
}
type Target struct {
	TargetInput
	ID             int64        `json:"id"`
	CreatedAt      int64        `json:"created_at"`
	NextCheckAt    int64        `json:"next_check_at"`
	Version        int64        `json:"-"`
	Latest         *CheckRecord `json:"latest"`
	Stats          SampleStats  `json:"stats"`
	Running        bool         `json:"running"`
	RuleValuesJSON []string     `json:"rule_values_json,omitempty"`
}
type CheckRecord struct {
	ID              int64           `json:"id"`
	TargetID        int64           `json:"target_id"`
	InputURL        string          `json:"input_url"`
	FinalURL        string          `json:"final_url"`
	StatusCode      int             `json:"status_code"`
	DurationMS      float64         `json:"duration_ms"`
	Kind            string          `json:"kind"`
	Detail          string          `json:"detail"`
	CheckedAt       int64           `json:"checked_at"`
	TotalDurationMS *float64        `json:"total_duration_ms"`
	ConfigVersion   *int64          `json:"config_version"`
	RulesSnapshot   json.RawMessage `json:"rules_snapshot"`
}

func validateTarget(in *TargetInput) error {
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	if utf8.RuneCountInString(in.Name) < 1 || utf8.RuneCountInString(in.Name) > 80 {
		return fmt.Errorf("%w：名称需要1–80个字符", ErrValidation)
	}
	if len(in.URL) > 2048 {
		return fmt.Errorf("%w：网址最长2048字节", ErrValidation)
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return fmt.Errorf("%w：需要不含用户凭据的完整HTTP/HTTPS网址", ErrValidation)
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%w：端口范围为1–65535", ErrValidation)
		}
	}
	if in.IntervalSeconds < 5 || in.IntervalSeconds > 86400 {
		return fmt.Errorf("%w：间隔范围为5–86400秒", ErrValidation)
	}
	if in.TimeoutSeconds < 1 || in.TimeoutSeconds > 30 {
		return fmt.Errorf("%w：超时范围为1–30秒", ErrValidation)
	}
	if _, err := parseRules(in.Rules); err != nil {
		return err
	}
	return nil
}
