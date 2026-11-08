package main

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This fixture deliberately bypasses OpenStore, so old and expired data cannot
// disappear before the behavior under test gets to read it.
func backupFixture(t *testing.T, version int, wal bool) (string, *sql.DB) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "source.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	script := schemaV1
	if version >= 2 {
		script += schemaV2
	}
	if version >= 3 {
		script += `ALTER TABLE targets ADD COLUMN rules TEXT NOT NULL DEFAULT '{}'; ALTER TABLE checks ADD COLUMN total_duration_ms REAL; ALTER TABLE checks ADD COLUMN config_version INTEGER; ALTER TABLE checks ADD COLUMN rules_snapshot TEXT;`
	}
	if version >= 4 {
		script += `ALTER TABLE checks ADD COLUMN error_category TEXT;`
	}
	if _, err = db.Exec(script); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA user_version=" + string(rune('0'+version))); err != nil {
		t.Fatal(err)
	}
	if wal {
		if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`INSERT INTO targets(id,name,url,interval_seconds,timeout_seconds,enabled,created_at,next_check_at,version) VALUES(42,'synthetic','https://example.test/?token=fake-backup-token',5,1,0,0,0,7);
 INSERT INTO checks(id,target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at) VALUES(99,42,'https://example.test','https://example.test',200,4,'ok','old sample',1);`); err != nil {
		t.Fatal(err)
	}
	if version >= 2 {
		if _, err = db.Exec(`INSERT INTO daily_summaries VALUES(42,'2001-01-01',8,7,50,10,1); INSERT INTO monitor_metadata VALUES('synthetic','preserved');`); err != nil {
			t.Fatal(err)
		}
	}
	if version >= 3 {
		if _, err = db.Exec(`UPDATE targets SET rules='{"body":{"kind":"contains","text":"synthetic-rule"}}'; UPDATE checks SET total_duration_ms=9.5,config_version=6,rules_snapshot='{"old":"synthetic-snapshot"}'`); err != nil {
			t.Fatal(err)
		}
	}
	if version >= 4 {
		if _, err = db.Exec(`UPDATE checks SET error_category='synthetic-category'`); err != nil {
			t.Fatal(err)
		}
	}
	return p, db
}

func backupRows(t *testing.T, db *sql.DB, table string) [][]any {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var all [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		for i, value := range values {
			if raw, ok := value.([]byte); ok {
				values[i] = append([]byte(nil), raw...)
			}
		}
		all = append(all, values)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return all
}

func TestBackupRoundTripPreservesWALAndLegacy(t *testing.T) {
	for _, v := range []int{1, 2, 3, 4} {
		t.Run(string(rune('0'+v)), func(t *testing.T) {
			source, db := backupFixture(t, v, true)
			if st, err := os.Stat(source + "-wal"); err != nil || st.Size() == 0 {
				t.Fatal("fixture has no WAL", err)
			}
			archive := filepath.Join(t.TempDir(), "backup.zip")
			manifest, err := BackupDatabase(context.Background(), source, archive)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.SchemaVersion != v || manifest.Counts["checks"] != 1 || !manifest.ContainsRawData || manifest.SnapshotSize <= 0 {
				t.Fatal(manifest)
			}
			verified, err := VerifyBackup(context.Background(), archive)
			if err != nil || verified.SHA256 != manifest.SHA256 {
				t.Fatal(verified, err)
			}
			dest := filepath.Join(t.TempDir(), "restored.db")
			if _, err = RestoreBackup(context.Background(), archive, dest); err != nil {
				t.Fatal(err)
			}
			restored, version, err := openMaintenanceSource(context.Background(), dest)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if version != v {
				t.Fatal(version)
			}
			tables := []string{"targets", "checks", "sqlite_sequence"}
			if v >= 2 {
				tables = append(tables, "daily_summaries", "monitor_metadata")
			}
			for _, table := range tables {
				if before, after := backupRows(t, db, table), backupRows(t, restored, table); !reflect.DeepEqual(before, after) {
					t.Fatalf("logical data changed in %s: %v vs %v", table, before, after)
				}
			}
			var id, checked int
			if err = restored.QueryRow("SELECT id,checked_at FROM checks").Scan(&id, &checked); err != nil || id != 99 || checked != 1 {
				t.Fatal(id, checked, err)
			}
			var originalVersion, originalCount int
			db.QueryRow("PRAGMA user_version").Scan(&originalVersion)
			db.QueryRow("SELECT COUNT(*) FROM checks").Scan(&originalCount)
			if originalVersion != v || originalCount != 1 {
				t.Fatal("source migrated or retained", originalVersion, originalCount)
			}
			if _, err = RestoreBackup(context.Background(), archive, dest); err == nil {
				t.Fatal("overwrote restored database")
			}
		})
	}
}

func TestBackupCancellationLocksCorruptionAndDestinations(t *testing.T) {
	source, db := backupFixture(t, 3, false)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := BackupDatabase(ctx, source, filepath.Join(dir, "cancel.zip")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := db.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := BackupDatabase(ctx, source, filepath.Join(dir, "locked.zip")); err == nil {
		t.Fatal("locked source succeeded")
	}
	db.Exec("ROLLBACK")
	corrupt := filepath.Join(dir, "corrupt.db")
	os.WriteFile(corrupt, []byte("not sqlite"), 0600)
	if _, err := BackupDatabase(context.Background(), corrupt, filepath.Join(dir, "corrupt.zip")); err == nil {
		t.Fatal("corruption accepted")
	}
	db.Exec("PRAGMA user_version=5")
	if _, err := BackupDatabase(context.Background(), source, filepath.Join(dir, "future.zip")); err == nil {
		t.Fatal("future accepted")
	}
	for _, name := range []string{"cancel.zip", "locked.zip", "corrupt.zip", "future.zip"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed operation left successful target", name, err)
		}
	}
}

func rewriteBackup(t *testing.T, input, output string, change func(*zip.FileHeader, []byte) (*zip.FileHeader, []byte), extra string) {
	t.Helper()
	src, err := zip.OpenReader(input)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	f, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	for _, entry := range src.File {
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		h := entry.FileHeader
		h.CRC32 = 0
		h.CompressedSize64 = 0
		h.UncompressedSize64 = 0
		h.Flags = 0
		hh, bb := change(&h, b)
		w, err := z.CreateHeader(hh)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(bb)
	}
	if extra != "" {
		w, _ := z.Create(extra)
		io.WriteString(w, "extra")
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestVerifyBackupRejectsUnsafeAndTamperedArchives(t *testing.T) {
	source, _ := backupFixture(t, 3, false)
	original := filepath.Join(t.TempDir(), "good.zip")
	if _, err := BackupDatabase(context.Background(), source, original); err != nil {
		t.Fatal(err)
	}
	tests := map[string]struct {
		change func(*zip.FileHeader, []byte) (*zip.FileHeader, []byte)
		extra  string
	}{
		"checksum": {change: func(h *zip.FileHeader, b []byte) (*zip.FileHeader, []byte) {
			if h.Name == "database.db" {
				b[len(b)-1] ^= 1
			}
			return h, b
		}},
		"path": {change: func(h *zip.FileHeader, b []byte) (*zip.FileHeader, []byte) {
			if h.Name == "database.db" {
				h.Name = "../database.db"
			}
			return h, b
		}},
		"symlink": {change: func(h *zip.FileHeader, b []byte) (*zip.FileHeader, []byte) {
			h.SetMode(os.ModeSymlink | 0777)
			return h, b
		}},
		"version": {change: func(h *zip.FileHeader, b []byte) (*zip.FileHeader, []byte) {
			if h.Name == "manifest.json" {
				var m map[string]any
				json.Unmarshal(b, &m)
				m["format_version"] = 99
				b, _ = json.Marshal(m)
			}
			return h, b
		}},
		"counts": {change: func(h *zip.FileHeader, b []byte) (*zip.FileHeader, []byte) {
			if h.Name == "manifest.json" {
				var m map[string]any
				json.Unmarshal(b, &m)
				m["counts"] = map[string]int{"checks": 999}
				b, _ = json.Marshal(m)
			}
			return h, b
		}},
		"bomb": {change: func(h *zip.FileHeader, b []byte) (*zip.FileHeader, []byte) {
			if h.Name == "manifest.json" {
				b = []byte(strings.Repeat(" ", 65537))
			}
			return h, b
		}},
		"duplicate": {extra: "database.db"}, "unknown": {extra: "unexpected"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			bad := filepath.Join(t.TempDir(), "bad.zip")
			change := test.change
			if change == nil {
				change = func(h *zip.FileHeader, b []byte) (*zip.FileHeader, []byte) { return h, b }
			}
			rewriteBackup(t, original, bad, change, test.extra)
			if _, err := VerifyBackup(context.Background(), bad); err == nil {
				t.Fatal("unsafe archive accepted")
			}
			dest := filepath.Join(t.TempDir(), "restore.db")
			if _, err := RestoreBackup(context.Background(), bad, dest); err == nil {
				t.Fatal("unsafe restore succeeded")
			}
			if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("restore target left", err)
			}
		})
	}
}

func TestBackupRejectsForeignKeyViolations(t *testing.T) {
	source, db := backupFixture(t, 3, false)
	if _, err := db.Exec("PRAGMA foreign_keys=OFF; UPDATE checks SET target_id=999"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "backup.zip")
	if _, err := BackupDatabase(context.Background(), source, dest); err == nil {
		t.Fatal("foreign key violation accepted")
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid backup published", err)
	}
}

func TestVerifyBackupRejectsTruncatedOversizeAndDirectoryFlood(t *testing.T) {
	source, _ := backupFixture(t, 3, false)
	good := filepath.Join(t.TempDir(), "good.zip")
	if _, err := BackupDatabase(context.Background(), source, good); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"truncated", "compressed", "uncompressed", "directory"} {
		t.Run(kind, func(t *testing.T) {
			bad := filepath.Join(t.TempDir(), "bad.zip")
			switch kind {
			case "truncated":
				err = os.WriteFile(bad, b[:len(b)-11], 0600)
			case "compressed":
				var f *os.File
				f, err = os.Create(bad)
				if err == nil {
					err = f.Truncate(backupArchiveLimit + 1)
					f.Close()
				}
			case "uncompressed":
				data := append([]byte(nil), b...)
				patched := false
				for i := 0; i+46 <= len(data); i++ {
					if binary.LittleEndian.Uint32(data[i:]) == 0x02014b50 && string(data[i+46:i+46+int(binary.LittleEndian.Uint16(data[i+28:]))]) == "database.db" {
						binary.LittleEndian.PutUint32(data[i+24:], uint32(backupExtractLimit+1))
						patched = true
						break
					}
				}
				if !patched {
					t.Fatal("missing central entry")
				}
				err = os.WriteFile(bad, data, 0600)
			case "directory":
				data := append([]byte(nil), b...)
				e := len(data) - 22
				binary.LittleEndian.PutUint16(data[e+8:], 65534)
				binary.LittleEndian.PutUint16(data[e+10:], 65534)
				err = os.WriteFile(bad, data, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyBackup(context.Background(), bad); err == nil {
				t.Fatal("bad archive accepted")
			}
		})
	}
}

func TestBackupZipPreflightRejectsForgedDirectoryAndZIP64(t *testing.T) {
	source, _ := backupFixture(t, 3, false)
	good := filepath.Join(t.TempDir(), "good.zip")
	if _, err := BackupDatabase(context.Background(), source, good); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"forged_size", "forged_count", "zip64_locator", "zip64_field", "trailing_data", "multidisk_entry", "offset"} {
		t.Run(kind, func(t *testing.T) {
			data := append([]byte(nil), original...)
			end := len(data) - 22
			offset := int(binary.LittleEndian.Uint32(data[end+16:]))
			size := int(binary.LittleEndian.Uint32(data[end+12:]))
			switch kind {
			case "forged_size":
				binary.LittleEndian.PutUint32(data[end+12:], 0)
			case "forged_count":
				firstLen := 46 + int(binary.LittleEndian.Uint16(data[offset+28:])) + int(binary.LittleEndian.Uint16(data[offset+30:])) + int(binary.LittleEndian.Uint16(data[offset+32:]))
				record := append([]byte(nil), data[offset:offset+firstLen]...)
				extra := make([]byte, 0, 65536*len(record))
				for range 65536 {
					extra = append(extra, record...)
				}
				data = append(append(append([]byte(nil), data[:end]...), extra...), data[end:]...)
				newEnd := len(data) - 22
				binary.LittleEndian.PutUint32(data[newEnd+12:], uint32(size+len(extra)))
				binary.LittleEndian.PutUint32(data[newEnd+12:], 0)
			case "zip64_locator":
				data = append(append(append([]byte(nil), data[:end]...), make([]byte, 20)...), data[end:]...)
				binary.LittleEndian.PutUint32(data[end:], 0x07064b50)
			case "zip64_field":
				binary.LittleEndian.PutUint32(data[offset+24:], 0xffffffff)
			case "trailing_data":
				data = append(data, []byte("unexpected tail")...)
			case "multidisk_entry":
				binary.LittleEndian.PutUint16(data[offset+34:], 1)
			case "offset":
				binary.LittleEndian.PutUint32(data[end+16:], 0)
			}
			path := filepath.Join(t.TempDir(), "bad.zip")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err = validateZipDirectory(f, int64(len(data))); err == nil {
				t.Fatal("unsafe ZIP passed allocation preflight")
			}
		})
	}
}
