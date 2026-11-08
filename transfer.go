package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const configurationFormatVersion = 1

// Four MiB accommodates 100 valid targets even when maximum-size rule strings
// consist of control characters that expand sixfold during JSON encoding.
const configurationInputLimit = 4 << 20

type configurationFile struct {
	FormatVersion         int           `json:"format_version"`
	GeneratedAt           time.Time     `json:"generated_at"`
	Redacted              bool          `json:"redacted"`
	ContainsSensitiveData bool          `json:"contains_sensitive_data"`
	Targets               []TargetInput `json:"targets"`
}

type ImportReport struct {
	SourceTargets   int    `json:"source_targets"`
	ImportedTargets int    `json:"imported_targets"`
	SourceEnabled   int    `json:"source_enabled"`
	EnabledTargets  int    `json:"enabled_targets"`
	KeepEnabled     bool   `json:"keep_enabled"`
	DryRun          bool   `json:"dry_run"`
	Policy          string `json:"policy"`
}

// ExportConfiguration never opens a maintenance source through OpenStore: that
// path migrates schemas and prunes records. A read transaction pins one snapshot.
func ExportConfiguration(ctx context.Context, source, dest string, full bool) error {
	db, version, err := openMaintenanceSource(ctx, source)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Re-read the schema version inside the same snapshot as the configurations.
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 1 || version > 4 {
		return fmt.Errorf("不支持数据库版本（支持1–4）")
	}
	rulesColumn := "'{}'"
	if version >= 3 {
		rulesColumn = "rules"
	}
	rows, err := tx.QueryContext(ctx, "SELECT name,url,interval_seconds,timeout_seconds,enabled,"+rulesColumn+" FROM targets ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	doc := configurationFile{FormatVersion: configurationFormatVersion, GeneratedAt: time.Now().UTC(), Redacted: !full, ContainsSensitiveData: full, Targets: []TargetInput{}}
	for rows.Next() {
		var in TargetInput
		var rules string
		if err = rows.Scan(&in.Name, &in.URL, &in.IntervalSeconds, &in.TimeoutSeconds, &in.Enabled, &rules); err != nil {
			return err
		}
		in.Rules = json.RawMessage(rules)
		if !full {
			in.URL = redactURL(in.URL)
		}
		doc.Targets = append(doc.Targets, in)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	return writeNewOutput(ctx, dest, func(w io.Writer) error { enc := json.NewEncoder(w); enc.SetIndent("", "  "); return enc.Encode(doc) })
}

// ImportConfiguration performs every format and target validation before any
// database creation. Imported configuration receives fresh IDs and no history.
func ImportConfiguration(ctx context.Context, input, dest string, keepEnabled, dryRun bool) (report ImportReport, err error) {
	report.KeepEnabled = keepEnabled
	report.DryRun = dryRun
	report.Policy = "all_paused"
	if keepEnabled {
		report.Policy = "preserve_source_enabled"
	}
	if err = ctx.Err(); err != nil {
		return report, err
	}
	if err = validateNewDatabasePath(dest); err != nil {
		return report, err
	}
	file, err := os.Open(input)
	if err != nil {
		return report, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, configurationInputLimit+1))
	if err != nil {
		return report, err
	}
	if len(raw) > configurationInputLimit {
		return report, fmt.Errorf("%w：配置文件最大4MiB", ErrValidation)
	}
	targets, err := parseConfiguration(ctx, raw)
	if err != nil {
		return report, err
	}
	report.SourceTargets = len(targets)
	for i := range targets {
		if targets[i].Enabled {
			report.SourceEnabled++
		}
		if !keepEnabled {
			targets[i].Enabled = false
		}
		if targets[i].Enabled {
			report.EnabledTargets++
		}
	}
	if dryRun {
		return report, nil
	}
	if err = ctx.Err(); err != nil {
		return report, err
	}
	temp, err := os.CreateTemp(filepath.Dir(dest), ".pulse-import-*.db")
	if err != nil {
		return report, err
	}
	tempPath := temp.Name()
	if err = temp.Close(); err != nil {
		os.Remove(tempPath)
		return report, err
	}
	defer func() { os.Remove(tempPath); os.Remove(tempPath + "-wal"); os.Remove(tempPath + "-shm") }()
	store, err := OpenStore(tempPath)
	if err != nil {
		return report, err
	}
	closed := false
	defer func() {
		if !closed {
			store.Close()
		}
	}()
	if err = importConfigurationTargets(ctx, store.db, targets); err != nil {
		return report, err
	}
	// Convert the private staging database to a single self-contained file before
	// publication. No live source is checkpointed or changed here.
	if _, err = store.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return report, err
	}
	var mode string
	if err = store.db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		return report, err
	}
	if mode != "delete" {
		return report, fmt.Errorf("无法完成导入数据库日志合并")
	}
	if err = store.Close(); err != nil {
		return report, err
	}
	closed = true
	staged, e := os.OpenFile(tempPath, os.O_RDWR, 0600)
	if e != nil {
		return report, e
	}
	if e = staged.Sync(); e != nil {
		staged.Close()
		return report, e
	}
	if e = staged.Close(); e != nil {
		return report, e
	}
	if err = ctx.Err(); err != nil {
		return report, err
	}
	if err = publishConfigurationDatabase(tempPath, dest); err != nil {
		return report, err
	}
	report.ImportedTargets = len(targets)
	return report, nil
}

func publishConfigurationDatabase(temp, dest string) error {
	if err := validateNewDatabasePath(dest); err != nil {
		return err
	}
	return publishNewFile(temp, dest)
}

func parseConfiguration(ctx context.Context, raw []byte) ([]TargetInput, error) {
	value, err := decodeStrictJSON(ctx, raw)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w：配置需要有效UTF-8 JSON且不能有重复字段", ErrValidation)
	}
	if configurationHasPlaceholder(value) {
		return nil, fmt.Errorf("%w：配置含REDACTED脱敏占位，须补齐真实配置", ErrValidation)
	}
	if !configurationKnownFields(value) {
		return nil, fmt.Errorf("%w：配置含未知字段或字段名大小写错误", ErrValidation)
	}
	var doc struct {
		FormatVersion         *int       `json:"format_version"`
		GeneratedAt           *time.Time `json:"generated_at"`
		Redacted              *bool      `json:"redacted"`
		ContainsSensitiveData *bool      `json:"contains_sensitive_data"`
		Targets               *[]struct {
			Name            *string         `json:"name"`
			URL             *string         `json:"url"`
			IntervalSeconds *int            `json:"interval_seconds"`
			TimeoutSeconds  *int            `json:"timeout_seconds"`
			Enabled         *bool           `json:"enabled"`
			Rules           json.RawMessage `json:"rules"`
		} `json:"targets"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w：配置字段类型、格式或未知字段无效", ErrValidation)
	}
	if doc.FormatVersion == nil || *doc.FormatVersion != configurationFormatVersion || doc.GeneratedAt == nil || doc.GeneratedAt.IsZero() || doc.Redacted == nil || doc.ContainsSensitiveData == nil || doc.Targets == nil {
		return nil, fmt.Errorf("%w：配置版本或必填元数据无效", ErrValidation)
	}
	if *doc.Redacted {
		return nil, fmt.Errorf("%w：脱敏文件不能直接导入，请补齐真实配置", ErrValidation)
	}
	if !*doc.ContainsSensitiveData {
		return nil, fmt.Errorf("%w：完整配置须明确标记可能包含敏感原始数据", ErrValidation)
	}
	if len(*doc.Targets) > 100 {
		return nil, ErrLimit
	}
	targets := make([]TargetInput, 0, len(*doc.Targets))
	seen := map[string]bool{}
	for i, target := range *doc.Targets {
		if target.Name == nil || target.URL == nil || target.IntervalSeconds == nil || target.TimeoutSeconds == nil || target.Enabled == nil {
			return nil, fmt.Errorf("%w：目标%d缺少必填配置", ErrValidation, i+1)
		}
		in := TargetInput{Name: *target.Name, URL: *target.URL, IntervalSeconds: *target.IntervalSeconds, TimeoutSeconds: *target.TimeoutSeconds, Enabled: *target.Enabled, Rules: target.Rules}
		if err = validateTarget(&in); err != nil {
			return nil, fmt.Errorf("目标%d：%w", i+1, err)
		}
		rules, e := parseRules(in.Rules)
		if e != nil {
			return nil, e
		}
		in.Rules, _ = json.Marshal(rules)
		normalized, _ := json.Marshal(in)
		if seen[string(normalized)] {
			return nil, fmt.Errorf("%w：目标%d与前面的规范化配置完全重复", ErrValidation, i+1)
		}
		seen[string(normalized)] = true
		targets = append(targets, in)
	}
	return targets, nil
}

func configurationHasPlaceholder(value any) bool {
	switch v := value.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "REDACTED") {
			return true
		}
		// A placeholder is a complete value, not a substring of a legitimate
		// name, hostname, path or assertion. Query parsing decodes encoded values.
		if u, err := url.Parse(v); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			// redactURL replaces an undecodable query-key part with a bare marker.
			// Inspect each complete part so this form cannot be imported either.
			for _, part := range strings.Split(u.RawQuery, "&") {
				if decoded, err := url.QueryUnescape(part); err == nil && strings.EqualFold(strings.TrimSpace(decoded), "REDACTED") {
					return true
				}
			}
			values, _ := url.ParseQuery(u.RawQuery)
			for _, repeated := range values {
				for _, item := range repeated {
					if strings.EqualFold(strings.TrimSpace(item), "REDACTED") {
						return true
					}
				}
			}
		}
		return false
	case []any:
		for _, item := range v {
			if configurationHasPlaceholder(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if configurationHasPlaceholder(item) {
				return true
			}
		}
	}
	return false
}

// encoding/json matches struct fields case-insensitively. Transfer files use an
// exact, versioned schema, so reject aliases instead of silently taking a value.
func configurationKnownFields(value any) bool {
	object, ok := value.(map[string]any)
	if !ok || !configurationKeys(object, "format_version", "generated_at", "redacted", "contains_sensitive_data", "targets") {
		return false
	}
	targets, ok := object["targets"].([]any)
	if !ok {
		return false
	}
	for _, item := range targets {
		target, ok := item.(map[string]any)
		if !ok || !configurationKeys(target, "name", "url", "interval_seconds", "timeout_seconds", "enabled", "rules") {
			return false
		}
		if raw, exists := target["rules"]; exists && raw != nil {
			rules, ok := raw.(map[string]any)
			if !ok || !configurationKeys(rules, "expected_statuses", "body_contains", "json_equals") {
				return false
			}
			if assertions, ok := rules["json_equals"].([]any); ok {
				for _, item := range assertions {
					assertion, ok := item.(map[string]any)
					if !ok || !configurationKeys(assertion, "pointer", "equals") {
						return false
					}
				}
			}
		}
	}
	return true
}

func configurationKeys(object map[string]any, names ...string) bool {
	for key := range object {
		found := false
		for _, name := range names {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func importConfigurationTargets(ctx context.Context, db *sql.DB, targets []TargetInput) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, in := range targets {
		rules := in.Rules
		if len(rules) == 0 {
			rules = json.RawMessage(`{}`)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO targets(name,url,interval_seconds,timeout_seconds,enabled,created_at,next_check_at,rules) VALUES(?,?,?,?,?,?,?,?)`, in.Name, in.URL, in.IntervalSeconds, in.TimeoutSeconds, in.Enabled, now, now, string(rules)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type historyExportMetadata struct {
	GeneratedAt        time.Time `json:"generated_at"`
	TargetID           int64     `json:"target_id"`
	RangeFromUTC       time.Time `json:"range_from_utc"`
	RangeToUTC         time.Time `json:"range_to_utc"`
	RetentionCutoffUTC time.Time `json:"retention_cutoff_utc"`
	TimeBasis          string    `json:"time_basis"`
	RangeBounds        string    `json:"range_bounds"`
	RetainedDataOnly   bool      `json:"retained_data_only"`
	Complete30Days     bool      `json:"complete_30_days"`
	Redacted           bool      `json:"redacted"`
	DurationUnit       string    `json:"duration_unit"`
	Notice             string    `json:"notice"`
}

// ExportHistory streams every retained matching row in one SQLite snapshot. It
// does not paginate and never fabricates legacy measurement/configuration data.
func ExportHistory(ctx context.Context, source, dest string, targetID int64, from, to time.Time, format string) error {
	if targetID < 1 || !from.Before(to) || (format != "json" && format != "csv") {
		return fmt.Errorf("%w：需现存目标、开始早于结束的时间范围以及json/csv格式", ErrValidation)
	}
	db, version, err := openMaintenanceSource(ctx, source)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 1 || version > 4 {
		return fmt.Errorf("不支持数据库版本（支持1–4）")
	}
	var exists int
	if err = tx.QueryRowContext(ctx, "SELECT 1 FROM targets WHERE id=?", targetID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	observed := time.Now().UTC()
	cutoff := observed.Add(-30 * 24 * time.Hour)
	meta := historyExportMetadata{GeneratedAt: observed, TargetID: targetID, RangeFromUTC: from.UTC(), RangeToUTC: to.UTC(), RetentionCutoffUTC: cutoff, TimeBasis: "checked_at", RangeBounds: "[from,to)", RetainedDataOnly: true, Complete30Days: false, Redacted: true, DurationUnit: "milliseconds", Notice: "仅导出仍保留且在滚动30天内的明细；每目标最多1000条，不代表完整30日，不支持导回为监测事实"}
	columns := `id,target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at,`
	if version >= 3 {
		columns += `total_duration_ms,config_version,rules_snapshot,`
	} else {
		columns += `NULL,NULL,NULL,`
	}
	if version >= 4 {
		columns += `error_category`
	} else {
		columns += `NULL`
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+columns+" FROM checks WHERE target_id=? AND checked_at>=? AND checked_at>=? AND checked_at<? ORDER BY checked_at,id", targetID, ceilMilliseconds(cutoff), ceilMilliseconds(from), ceilMilliseconds(to))
	if err != nil {
		return err
	}
	defer rows.Close()
	return writeNewOutput(ctx, dest, func(w io.Writer) error {
		if format == "csv" {
			return writeHistoryCSV(ctx, w, rows, meta)
		}
		if _, err = io.WriteString(w, `{"format_version":1,"metadata":`); err != nil {
			return err
		}
		enc := json.NewEncoder(w)
		if err = enc.Encode(meta); err != nil {
			return err
		}
		if _, err = io.WriteString(w, `,"checks":[`); err != nil {
			return err
		}
		first := true
		for rows.Next() {
			if err = ctx.Err(); err != nil {
				return err
			}
			record, e := scanCheck(rows)
			if e != nil {
				return e
			}
			if !first {
				if _, err = io.WriteString(w, ","); err != nil {
					return err
				}
			}
			first = false
			if err = enc.Encode(safeCheck(record)); err != nil {
				return err
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
		_, err = io.WriteString(w, "]}\n")
		return err
	})
}

func ceilMilliseconds(t time.Time) int64 {
	ms := t.UnixMilli()
	if t.Nanosecond()%int(time.Millisecond) != 0 {
		ms++
	}
	return ms
}

var historyCSVColumns = []string{"record_type", "export_format_version", "export_target_id", "range_from_utc", "range_to_utc", "retention_cutoff_utc", "retained_data_only", "complete_30_days", "range_bounds", "time_basis", "notice", "id", "target_id", "input_url", "final_url", "status_code", "duration_ms", "kind", "error_category", "detail", "checked_at_unix_ms", "checked_at_utc", "total_duration_ms", "config_version", "rules_snapshot"}

func writeHistoryCSV(ctx context.Context, w io.Writer, rows *sql.Rows, meta historyExportMetadata) error {
	writer := csv.NewWriter(w)
	if err := writer.Write(historyCSVColumns); err != nil {
		return err
	}
	prefix := []string{"metadata", "1", strconv.FormatInt(meta.TargetID, 10), meta.RangeFromUTC.Format(time.RFC3339Nano), meta.RangeToUTC.Format(time.RFC3339Nano), meta.RetentionCutoffUTC.Format(time.RFC3339Nano), "true", "false", meta.RangeBounds, meta.TimeBasis, meta.Notice}
	metadata := append(append([]string{}, prefix...), make([]string, len(historyCSVColumns)-len(prefix))...)
	if err := writer.Write(metadata); err != nil {
		return err
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, err := scanCheck(rows)
		if err != nil {
			return err
		}
		r = safeCheck(r)
		total, version, snapshot := "", "", ""
		if r.TotalDurationMS != nil {
			total = strconv.FormatFloat(*r.TotalDurationMS, 'g', -1, 64)
		}
		if r.ConfigVersion != nil {
			version = strconv.FormatInt(*r.ConfigVersion, 10)
		}
		if r.RulesSnapshot != nil {
			snapshot = string(r.RulesSnapshot)
		}
		values := append([]string{}, prefix...)
		values[0] = "check"
		values = append(values, strconv.FormatInt(r.ID, 10), strconv.FormatInt(r.TargetID, 10), csvSafeText(r.InputURL), csvSafeText(r.FinalURL), strconv.Itoa(r.StatusCode), strconv.FormatFloat(r.DurationMS, 'g', -1, 64), csvSafeText(r.Kind), csvSafeText(r.ErrorCategory), csvSafeText(r.Detail), strconv.FormatInt(r.CheckedAt, 10), csvSafeText(time.UnixMilli(r.CheckedAt).UTC().Format(time.RFC3339Nano)), total, version, csvSafeText(snapshot))
		for i := 0; i < len(prefix); i++ {
			values[i] = csvSafeText(values[i])
		}
		if err = writer.Write(values); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writer.Flush()
	return writer.Error()
}

// Quote formula introducers even after whitespace/control characters, including
// tab/CR/LF prefixes interpreted by spreadsheet importers.
func csvSafeText(text string) string {
	trimmed := strings.TrimLeftFunc(text, func(r rune) bool { return r <= 32 || unicode.IsSpace(r) || r == '\uFEFF' })
	if strings.ContainsAny(text, "\t\r\n") || strings.HasPrefix(trimmed, "=") || strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "@") {
		return "'" + text
	}
	return text
}
