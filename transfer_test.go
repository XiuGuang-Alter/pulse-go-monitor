package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func transferFixture(t *testing.T) (string, *Store, Target) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	in := inputFor("https://example.test/path?token=fake-secret#fragment")
	in.Rules = json.RawMessage(`{"json_equals":[{"pointer":"/n","equals":9007199254740993}]}`)
	target, err := s.SaveTarget(context.Background(), 0, in)
	if err != nil {
		t.Fatal(err)
	}
	return path, s, target
}

func transferDocument(t *testing.T, targets []TargetInput) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"format_version": 1, "generated_at": "2026-11-08T00:00:00Z", "redacted": false, "contains_sensitive_data": true, "targets": targets})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "input.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTransferConfigurationRoundTrip(t *testing.T) {
	ctx := context.Background()
	source, _, target := transferFixture(t)
	safe := filepath.Join(t.TempDir(), "safe.json")
	if err := ExportConfiguration(ctx, source, safe, false); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(safe)
	if strings.Contains(string(raw), "fake-secret") || !strings.Contains(string(raw), `"redacted": true`) {
		t.Fatalf("unsafe export %s", raw)
	}
	if _, err := ImportConfiguration(ctx, safe, filepath.Join(t.TempDir(), "bad.db"), false, false); err == nil {
		t.Fatal("redacted file imported")
	}
	full := filepath.Join(t.TempDir(), "full.json")
	if err := ExportConfiguration(ctx, source, full, true); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "new.db")
	report, err := ImportConfiguration(ctx, full, dest, false, true)
	if err != nil || report.SourceTargets != 1 || report.EnabledTargets != 0 || !report.DryRun {
		t.Fatalf("preflight %+v %v", report, err)
	}
	if _, err = os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry run created database")
	}
	report, err = ImportConfiguration(ctx, full, dest, false, false)
	if err != nil || report.ImportedTargets != 1 {
		t.Fatalf("import %+v %v", report, err)
	}
	db, err := OpenStore(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	list, err := db.ListTargets(ctx)
	if err != nil || len(list) != 1 || list[0].Enabled || list[0].URL != target.URL || !strings.Contains(string(list[0].Rules), "9007199254740993") {
		t.Fatalf("roundtrip %+v %v", list, err)
	}
	keep := filepath.Join(t.TempDir(), "keep.db")
	report, err = ImportConfiguration(ctx, full, keep, true, false)
	if err != nil || report.EnabledTargets != 1 {
		t.Fatalf("keep enabled %+v %v", report, err)
	}
	if _, err = ImportConfiguration(ctx, full, dest, false, false); err == nil {
		t.Fatal("overwrote destination")
	}
}

func TestTransferConfigurationValidation(t *testing.T) {
	ctx := context.Background()
	in := inputFor("https://example.test")
	cases := map[string][]TargetInput{"duplicate": {in, in}, "too_many": make([]TargetInput, 101), "redacted_url": {inputFor("https://example.test/?token=REDACTED")}}
	badRules := in
	badRules.Rules = json.RawMessage(`{"json_equals":[{"pointer":"bad","equals":1}]}`)
	cases["rules"] = []TargetInput{badRules}
	for name, inputs := range cases {
		t.Run(name, func(t *testing.T) {
			path := transferDocument(t, inputs)
			dest := filepath.Join(t.TempDir(), "new.db")
			if _, err := ImportConfiguration(ctx, path, dest, false, false); err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failure left destination")
			}
		})
	}
	other := in
	other.Rules = json.RawMessage(`{"body_contains":"ok"}`)
	if _, err := ImportConfiguration(ctx, transferDocument(t, []TargetInput{in, other}), filepath.Join(t.TempDir(), "new.db"), false, false); err != nil {
		t.Fatal("same URL with different rules rejected", err)
	}
	base := `{"format_version":1,"generated_at":"2026-11-08T00:00:00Z","redacted":false,"contains_sensitive_data":true,"targets":[]}`
	for name, text := range map[string]string{"unknown": strings.Replace(base, `"targets":[]`, `"targets":[],"mystery":1`, 1), "version": strings.Replace(base, `"format_version":1`, `"format_version":2`, 1), "duplicate_key": strings.Replace(base, `"redacted":false`, `"redacted":false,"redacted":false`, 1), "missing": `{"format_version":1,"targets":[]}`, "oversize": strings.Repeat(" ", 4*1024*1024+1) + base, "missing_target_field": strings.Replace(base, `"targets":[]`, `"targets":[{"name":"x","url":"https://example.test","interval_seconds":5,"timeout_seconds":1}]`, 1)} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "in.json")
			os.WriteFile(path, []byte(text), 0600)
			if _, err := ImportConfiguration(ctx, path, filepath.Join(t.TempDir(), "new.db"), false, false); err == nil {
				t.Fatal("invalid JSON accepted")
			}
		})
	}
	a := in
	a.Rules = json.RawMessage(`{"expected_statuses":[201,200,200]}`)
	b := in
	b.Rules = json.RawMessage(`{"expected_statuses":[200,201]}`)
	if _, err := ImportConfiguration(ctx, transferDocument(t, []TargetInput{a, b}), filepath.Join(t.TempDir(), "new.db"), false, false); err == nil {
		t.Fatal("normalized duplicate accepted")
	}
}

func TestTransferImportTransactionRollback(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_second BEFORE INSERT ON targets WHEN NEW.name='second' BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	first := inputFor("https://example.test")
	first.Name = "first"
	second := first
	second.Name = "second"
	if err := importConfigurationTargets(ctx, s.db, []TargetInput{first, second}); err == nil {
		t.Fatal("expected second insertion failure")
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM targets`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial targets count=%d err=%v", count, err)
	}
}

func TestTransferStrictFieldNamesAndPlaceholders(t *testing.T) {
	base := `{"format_version":1,"generated_at":"2026-11-08T00:00:00Z","redacted":false,"contains_sensitive_data":true,"targets":[{"name":"x","url":"https://example.test","interval_seconds":5,"timeout_seconds":1,"enabled":false,"rules":{}}]}`
	for name, text := range map[string]string{
		"envelope_case":                strings.Replace(base, `"redacted"`, `"Redacted"`, 1),
		"target_case":                  strings.Replace(base, `"name"`, `"Name"`, 1),
		"rule_case":                    strings.Replace(base, `"rules":{}`, `"rules":{"Body_contains":"ok"}`, 1),
		"rule_unknown":                 strings.Replace(base, `"rules":{}`, `"rules":{"unexpected":1}`, 1),
		"encoded_placeholder":          strings.Replace(base, "https://example.test", "https://example.test/?token=%52EDACTED", 1),
		"bare_query_placeholder":       strings.Replace(base, "https://example.test", "https://example.test/?REDACTED", 1),
		"encoded_bare_placeholder":     strings.Replace(base, "https://example.test", "https://example.test/?safe=1&%52EDACTED", 1),
		"rule_placeholder":             strings.Replace(base, `"rules":{}`, `"rules":{"body_contains":"REDACTED"}`, 1),
		"contradictory_sensitive_flag": strings.Replace(base, `"contains_sensitive_data":true`, `"contains_sensitive_data":false`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "in.json")
			os.WriteFile(path, []byte(text), 0600)
			if _, err := ImportConfiguration(context.Background(), path, filepath.Join(t.TempDir(), "new.db"), false, false); err == nil {
				t.Fatal("strict configuration accepted invalid fields/placeholders")
			}
		})
	}
}

func TestTransferPublicationRechecksSQLiteSidecars(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			dir := t.TempDir()
			temp := filepath.Join(dir, "staged.db")
			dest := filepath.Join(dir, "target.db")
			if err := os.WriteFile(temp, []byte("staged"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dest+suffix, []byte("other operation"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := publishConfigurationDatabase(temp, dest); err == nil {
				t.Fatal("published alongside existing SQLite sidecar")
			}
			if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("destination published despite sidecar")
			}
		})
	}
}

func TestTransferCancellationAndFilePublicationFailure(t *testing.T) {
	source, _, _ := transferFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := filepath.Join(t.TempDir(), "cancelled.json")
	if err := ExportConfiguration(ctx, source, path, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled export published")
	}
	existing := filepath.Join(t.TempDir(), "existing.json")
	os.WriteFile(existing, []byte("sentinel"), 0600)
	if err := ExportConfiguration(context.Background(), source, existing, true); err == nil {
		t.Fatal("overwrote export")
	}
	b, _ := os.ReadFile(existing)
	if string(b) != "sentinel" {
		t.Fatal("existing file changed")
	}
	dir := t.TempDir()
	full := filepath.Join(t.TempDir(), "full.json")
	if err := ExportConfiguration(context.Background(), source, full, true); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportConfiguration(ctx, full, filepath.Join(dir, "cancelled.db"), false, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("import cancellation %v", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("cancelled import left files %v %v", files, err)
	}
	dest := filepath.Join(dir, "missing-parent", "new.db")
	if _, err = ImportConfiguration(context.Background(), full, dest, false, false); err == nil {
		t.Fatal("missing output directory accepted")
	}
	files, err = os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("failed import left files %v %v", files, err)
	}
}

func TestTransferCSVProtectsAllTextIntroducers(t *testing.T) {
	for _, text := range []string{"=1+1", "+cmd", "-cmd", "@cmd", " \t=cmd", "\r=cmd", "\n=cmd", "\u00a0=cmd", "\ufeff=cmd"} {
		if got := csvSafeText(text); got != "'"+text {
			t.Fatalf("formula not neutralized %q -> %q", text, got)
		}
	}
}

func TestTransferPlaceholderDetectionAllowsLegitimateSubstrings(t *testing.T) {
	in := inputFor("https://redacted.example.test/?token=unredacted-value")
	in.Name = "unredacted target"
	in.Rules = json.RawMessage(`{"body_contains":"unredacted","json_equals":[{"pointer":"/redactedValue","equals":"not REDACTED here"}]}`)
	if _, err := ImportConfiguration(context.Background(), transferDocument(t, []TargetInput{in}), filepath.Join(t.TempDir(), "new.db"), false, true); err != nil {
		t.Fatalf("legitimate redacted substring rejected: %v", err)
	}
}

func TestTransferMaximumLegalEscapedConfigurationRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := filepath.Join(t.TempDir(), "max.db")
	s, err := OpenStore(source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	equals, _ := json.Marshal(strings.Repeat("\x00", 256))
	rules := CheckRules{BodyContains: strings.Repeat("\x00", 256)}
	for i := 0; i < 5; i++ {
		rules.JSONEquals = append(rules.JSONEquals, JSONAssertion{Pointer: "/" + strings.Repeat("\x00", 255), Equals: equals})
	}
	rawRules, _ := json.Marshal(rules)
	for i := 0; i < 100; i++ {
		in := inputFor("https://example.test")
		in.Name = fmt.Sprintf("escaped %d", i)
		in.Rules = rawRules
		if _, err = s.SaveTarget(ctx, 0, in); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(t.TempDir(), "large.json")
	if err = ExportConfiguration(ctx, source, output, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Size() <= 1<<20 {
		t.Fatalf("fixture did not exercise JSON expansion: %v %v", info, err)
	}
	if report, err := ImportConfiguration(ctx, output, filepath.Join(t.TempDir(), "new.db"), true, false); err != nil || report.ImportedTargets != 100 {
		t.Fatalf("maximum legal configuration roundtrip %+v %v", report, err)
	}
}

func TestTransferHistoryRangePrecisionAndCSV(t *testing.T) {
	ctx := context.Background()
	source, s, target := transferFixture(t)
	from := time.Now().UTC().Truncate(time.Second)
	to := from.Add(time.Hour)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 450; i++ {
		_, err = tx.Exec(`INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at,config_version,rules_snapshot) VALUES(?,?,?,?,?,?,?,?,?,?)`, target.ID, target.URL, target.URL, 200, 1.25, "=danger", "old secret", from.Add(time.Duration(i)*time.Second).UnixMilli(), int64(9007199254740993), `{"json_equals":[{"pointer":"/n","equals":9007199254740993}]}`)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []time.Time{from.Add(-time.Millisecond), to, time.Now().Add(-31 * 24 * time.Hour)} {
		if _, err = tx.Exec(`INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at) VALUES(?,?,?,?,?,?,?,?)`, target.ID, target.URL, target.URL, 500, 2, "http_error", "secret", at.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "history.json")
	if err = ExportHistory(ctx, source, dest, target.ID, from, to, "json"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(dest)
	var data struct {
		Checks   []CheckRecord  `json:"checks"`
		Metadata map[string]any `json:"metadata"`
	}
	if err = json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Checks) != 450 || data.Checks[0].CheckedAt != from.UnixMilli() || data.Checks[0].TotalDurationMS != nil || data.Checks[0].ConfigVersion == nil || *data.Checks[0].ConfigVersion != 9007199254740993 || data.Metadata["retained_data_only"] != true {
		t.Fatalf("history count=%d metadata=%v", len(data.Checks), data.Metadata)
	}
	if strings.Contains(string(raw), "fake-secret") || strings.Contains(string(raw), "old secret") {
		t.Fatal("history leaks secret")
	}
	// checked_at stores integer milliseconds; a nanosecond range boundary must
	// still follow the precise left-closed/right-open comparison.
	fine := filepath.Join(t.TempDir(), "fine.json")
	if err = ExportHistory(ctx, source, fine, target.ID, from.Add(time.Nanosecond), from.Add(time.Second+time.Nanosecond), "json"); err != nil {
		t.Fatal(err)
	}
	fineRaw, _ := os.ReadFile(fine)
	var fineData struct {
		Checks []CheckRecord `json:"checks"`
	}
	if err = json.Unmarshal(fineRaw, &fineData); err != nil || len(fineData.Checks) != 1 || fineData.Checks[0].CheckedAt != from.Add(time.Second).UnixMilli() {
		t.Fatalf("sub-millisecond range %+v %v", fineData, err)
	}
	csvPath := filepath.Join(t.TempDir(), "history.csv")
	if err = ExportHistory(ctx, source, csvPath, target.ID, from, to, "csv"); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 452 {
		t.Fatalf("CSV rows %d", len(records))
	}
	indexes := map[string]int{}
	for i, name := range records[0] {
		indexes[name] = i
	}
	if records[1][indexes["record_type"]] != "metadata" || records[1][indexes["id"]] != "" || records[2][indexes["kind"]] != "'=danger" || records[2][indexes["config_version"]] != "9007199254740993" || records[2][indexes["total_duration_ms"]] != "" {
		t.Fatalf("CSV malformed %+v", records[:3])
	}
	if records[1][indexes["export_target_id"]] != "1" {
		t.Fatal("CSV metadata lost selected target")
	}
	empty := filepath.Join(t.TempDir(), "empty.csv")
	if err = ExportHistory(ctx, source, empty, target.ID, to, to.Add(time.Hour), "csv"); err != nil {
		t.Fatal(err)
	}
	emptyRaw, _ := os.ReadFile(empty)
	if !strings.Contains(string(emptyRaw), "metadata") || !strings.Contains(string(emptyRaw), to.Format(time.RFC3339Nano)) {
		t.Fatal("empty CSV lost range")
	}
	for _, tc := range []struct {
		id       int64
		from, to time.Time
		format   string
	}{{target.ID, to, from, "json"}, {target.ID, from, to, "xml"}, {999, from, to, "json"}} {
		if err = ExportHistory(ctx, source, filepath.Join(t.TempDir(), "bad"), tc.id, tc.from, tc.to, tc.format); err == nil {
			t.Fatal("invalid history request accepted")
		}
	}
}

func TestTransferLegacyReadOnly(t *testing.T) {
	for version := 1; version <= 4; version++ {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err = db.Exec(schemaV1); err != nil {
				t.Fatal(err)
			}
			if version >= 2 {
				if _, err = db.Exec(schemaV2); err != nil {
					t.Fatal(err)
				}
			}
			if version >= 3 {
				if _, err = db.Exec(`ALTER TABLE targets ADD COLUMN rules TEXT NOT NULL DEFAULT '{}';ALTER TABLE checks ADD COLUMN total_duration_ms REAL;ALTER TABLE checks ADD COLUMN config_version INTEGER;ALTER TABLE checks ADD COLUMN rules_snapshot TEXT;`); err != nil {
					t.Fatal(err)
				}
			}
			if version >= 4 {
				if _, err = db.Exec(`ALTER TABLE checks ADD COLUMN error_category TEXT`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = db.Exec(fmt.Sprintf(`PRAGMA user_version=%d;INSERT INTO targets(name,url,interval_seconds,timeout_seconds,enabled,created_at,next_check_at) VALUES('old','https://example.test',5,1,1,0,0);INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at) VALUES(1,'https://example.test','',200,1,'ok','secret',0)`, version)); err != nil {
				t.Fatal(err)
			}
			if err = ExportConfiguration(context.Background(), path, filepath.Join(t.TempDir(), "config.json"), true); err != nil {
				t.Fatal(err)
			}
			if err = ExportHistory(context.Background(), path, filepath.Join(t.TempDir(), "history.json"), 1, time.Unix(0, 0), time.Now(), "json"); err != nil {
				t.Fatal(err)
			}
			var got, count int
			db.QueryRow("PRAGMA user_version").Scan(&got)
			db.QueryRow("SELECT count(*) FROM checks").Scan(&count)
			if got != version || count != 1 {
				t.Fatalf("source modified version=%d count=%d", got, count)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "missing.db")
	if err := ExportConfiguration(context.Background(), missing, filepath.Join(t.TempDir(), "out"), true); err == nil {
		t.Fatal("missing source accepted")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing source created")
	}
}
