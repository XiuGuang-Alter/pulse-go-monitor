package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"modernc.org/sqlite"
)

const (
	backupFormatVersion       = 1
	backupArchiveLimit  int64 = 256 << 20
	backupExtractLimit  int64 = 256 << 20
	backupManifestLimit int64 = 64 << 10
)

// BackupManifest describes the exact unmodified SQLite snapshot. Backups retain
// original URLs, rules, historical details, IDs, and other potentially private data.
type BackupManifest struct {
	FormatVersion   int              `json:"format_version"`
	CreatedAt       time.Time        `json:"created_at"`
	AppVersion      string           `json:"app_version"`
	SchemaVersion   int              `json:"schema_version"`
	Counts          map[string]int64 `json:"counts"`
	SnapshotSize    int64            `json:"snapshot_size"`
	SHA256          string           `json:"sha256"`
	ContainsRawData bool             `json:"contains_raw_data"`
}

func BackupDatabase(ctx context.Context, source, dest string) (BackupManifest, error) {
	var empty BackupManifest
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if _, err := os.Lstat(dest); err == nil {
		return empty, errors.New("备份目标已经存在")
	} else if !errors.Is(err, os.ErrNotExist) {
		return empty, err
	}
	db, _, err := openMaintenanceSource(ctx, source)
	if err != nil {
		return empty, err
	}
	defer db.Close()
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".pulse-backup-*")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(dir)
	snapshot := filepath.Join(dir, "database.db")
	if err = sqliteSnapshot(ctx, db, snapshot); err != nil {
		return empty, err
	}
	manifest, err := inspectSnapshot(ctx, snapshot)
	if err != nil {
		return empty, err
	}
	manifest.FormatVersion = backupFormatVersion
	manifest.CreatedAt = time.Now().UTC()
	manifest.AppVersion = "1.2.0-dev"
	manifest.ContainsRawData = true
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return empty, err
	}
	if int64(len(encoded)) > backupManifestLimit || manifest.SnapshotSize+int64(len(encoded)) > backupExtractLimit {
		return empty, errors.New("备份超过解压预算（256 MiB）")
	}
	err = writeNewOutput(ctx, dest, func(w io.Writer) error {
		limited := &budgetWriter{w: w, left: backupArchiveLimit}
		z := zip.NewWriter(limited)
		entry, e := z.Create("manifest.json")
		if e != nil {
			return e
		}
		if _, e = entry.Write(encoded); e != nil {
			return e
		}
		entry, e = z.Create("database.db")
		if e != nil {
			return e
		}
		f, e := os.Open(snapshot)
		if e != nil {
			return e
		}
		_, e = io.Copy(entry, contextReader{ctx: ctx, r: f})
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		return z.Close()
	})
	if err != nil {
		return empty, err
	}
	return manifest, nil
}

// modernc.org/sqlite v1.61.1 exposes SQLite's native online Backup API through
// Conn.Raw. Small Step calls bound cancellation latency; SQLITE_BUSY/LOCKED fail
// immediately (the source has busy_timeout=0). Finish closes the destination even
// after an interrupted/failed step. No source SQL writes or migration are used.
func sqliteSnapshot(ctx context.Context, db *sql.DB, dest string) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var pageCount, pageSize int64
	if err = conn.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return err
	}
	if err = conn.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return err
	}
	if pageSize <= 0 || pageCount > backupExtractLimit/pageSize {
		return errors.New("数据库快照超过 256 MiB 预算")
	}
	err = conn.Raw(func(raw any) (result error) {
		driver, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite 驱动不支持一致备份")
		}
		if result = ctx.Err(); result != nil {
			return result
		}
		backup, e := driver.NewBackup(dest)
		if e != nil {
			return e
		}
		defer func() { result = errors.Join(result, backup.Finish()) }()
		for {
			if e = ctx.Err(); e != nil {
				return e
			}
			more, e := backup.Step(32)
			if e != nil {
				return e
			}
			if int64(backup.PageCount()) > backupExtractLimit/pageSize {
				return errors.New("数据库快照超过 256 MiB 预算")
			}
			if !more {
				return ctx.Err()
			}
		}
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dest, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = f.Chmod(0600)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func VerifyBackup(ctx context.Context, archive string) (BackupManifest, error) {
	var empty BackupManifest
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	dir, err := os.MkdirTemp("", ".pulse-verify-*")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(dir)
	return extractVerifiedBackup(ctx, archive, filepath.Join(dir, "database.db"))
}

func RestoreBackup(ctx context.Context, archive, dest string) (BackupManifest, error) {
	var empty BackupManifest
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := validateNewDatabasePath(dest); err != nil {
		return empty, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".pulse-restore-*")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(dir)
	snapshot := filepath.Join(dir, "database.db")
	manifest, err := extractVerifiedBackup(ctx, archive, snapshot)
	if err != nil {
		return empty, err
	}
	// SQLite verification has closed all handles before publication. Check sidecars
	// again immediately before the atomic no-clobber operation.
	if err = validateNewDatabasePath(dest); err != nil {
		return empty, err
	}
	if err = ctx.Err(); err != nil {
		return empty, err
	}
	if err = publishNewFile(snapshot, dest); err != nil {
		return empty, err
	}
	return manifest, nil
}

func extractVerifiedBackup(ctx context.Context, archive, snapshot string) (BackupManifest, error) {
	var empty BackupManifest
	f, err := os.Open(archive)
	if err != nil {
		return empty, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return empty, err
	}
	if !info.Mode().IsRegular() || info.Size() > backupArchiveLimit || info.Size() < 22 {
		return empty, errors.New("备份归档无效或超过 256 MiB 压缩预算")
	}
	if err = validateZipDirectory(f, info.Size()); err != nil {
		return empty, err
	}
	z, err := zip.NewReader(f, info.Size())
	if err != nil {
		return empty, errors.New("备份归档损坏")
	}
	if len(z.File) != 2 {
		return empty, errors.New("备份归档必须仅含 manifest.json 和 database.db")
	}
	entries := make(map[string]*zip.File, 2)
	var total uint64
	for _, entry := range z.File {
		if entry.Name != "manifest.json" && entry.Name != "database.db" {
			return empty, errors.New("备份包含不允许的归档路径")
		}
		if entries[entry.Name] != nil {
			return empty, errors.New("备份含重复归档条目")
		}
		if !entry.Mode().IsRegular() || entry.Flags&1 != 0 || (entry.Method != zip.Store && entry.Method != zip.Deflate) {
			return empty, errors.New("备份含不允许的条目类型")
		}
		if entry.UncompressedSize64 > uint64(backupExtractLimit) || entry.CompressedSize64 > uint64(backupArchiveLimit) {
			return empty, errors.New("备份条目超过大小预算")
		}
		total += entry.UncompressedSize64
		if total > uint64(backupExtractLimit) {
			return empty, errors.New("备份超过 256 MiB 解压预算")
		}
		entries[entry.Name] = entry
	}
	metadata := entries["manifest.json"]
	if metadata == nil || entries["database.db"] == nil || metadata.UncompressedSize64 > uint64(backupManifestLimit) {
		return empty, errors.New("备份清单缺失或超过 64 KiB 预算")
	}
	r, err := metadata.Open()
	if err != nil {
		return empty, errors.New("读取备份清单失败")
	}
	encoded, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, r: r}, backupManifestLimit+1))
	closeErr := r.Close()
	if err != nil {
		return empty, err
	}
	if closeErr != nil {
		return empty, closeErr
	}
	if int64(len(encoded)) > backupManifestLimit {
		return empty, errors.New("备份清单超过 64 KiB 预算")
	}
	var manifest BackupManifest
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&manifest); err != nil {
		return empty, errors.New("备份清单格式无效")
	}
	if err = decoder.Decode(&struct{}{}); err != io.EOF {
		return empty, errors.New("备份清单含额外内容")
	}
	if manifest.FormatVersion != backupFormatVersion || manifest.SchemaVersion < 1 || manifest.SchemaVersion > 4 {
		return empty, errors.New("不支持备份格式或数据库版本")
	}
	_, offset := manifest.CreatedAt.Zone()
	if manifest.CreatedAt.IsZero() || offset != 0 || manifest.AppVersion == "" || !manifest.ContainsRawData || manifest.SnapshotSize <= 0 || manifest.SnapshotSize > backupExtractLimit || manifest.SnapshotSize != int64(entries["database.db"].UncompressedSize64) {
		return empty, errors.New("备份清单元数据无效")
	}
	digest, err := hex.DecodeString(manifest.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return empty, errors.New("备份校验和无效")
	}
	r, err = entries["database.db"].Open()
	if err != nil {
		return empty, errors.New("读取备份数据库失败")
	}
	err = writeNewOutput(ctx, snapshot, func(w io.Writer) error {
		n, e := io.Copy(w, io.LimitReader(contextReader{ctx: ctx, r: r}, manifest.SnapshotSize+1))
		if e != nil {
			return e
		}
		if n != manifest.SnapshotSize {
			return errors.New("备份数据库大小不匹配")
		}
		return nil
	})
	closeErr = r.Close()
	if err != nil {
		return empty, err
	}
	if closeErr != nil {
		return empty, closeErr
	}
	actual, err := inspectSnapshot(ctx, snapshot)
	if err != nil {
		return empty, err
	}
	if actual.SnapshotSize != manifest.SnapshotSize || actual.SHA256 != manifest.SHA256 || actual.SchemaVersion != manifest.SchemaVersion || !reflect.DeepEqual(actual.Counts, manifest.Counts) {
		return empty, errors.New("备份校验和、版本或数据计数不匹配")
	}
	return manifest, nil
}

// Refuse an excessive central directory before archive/zip allocates its entry
// list. This format has exactly two files and needs no ZIP64 or multi-disk data.
func validateZipDirectory(f *os.File, size int64) error {
	n := int64(65557)
	if size < n {
		n = size
	}
	tail := make([]byte, n)
	if _, err := f.ReadAt(tail, size-n); err != nil {
		return err
	}
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != 0x06054b50 {
			continue
		}
		e := tail[i:]
		if len(e) != 22+int(binary.LittleEndian.Uint16(e[20:])) {
			continue
		}
		if binary.LittleEndian.Uint16(e[4:]) != 0 || binary.LittleEndian.Uint16(e[6:]) != 0 || binary.LittleEndian.Uint16(e[8:]) != 2 || binary.LittleEndian.Uint16(e[10:]) != 2 || binary.LittleEndian.Uint32(e[12:]) > 64<<10 {
			return errors.New("备份 ZIP 目录或条目数量无效")
		}
		directorySize := int64(binary.LittleEndian.Uint32(e[12:]))
		directoryOffset := int64(binary.LittleEndian.Uint32(e[16:]))
		endOffset := size - n + int64(i)
		if directorySize < 92 || directoryOffset <= 0 || directoryOffset+directorySize != endOffset {
			return errors.New("备份 ZIP 目录范围无效")
		}
		// Do not let archive/zip infer a prefixed archive or follow a ZIP64
		// locator. Parse the complete bounded central directory ourselves first.
		directory := make([]byte, directorySize)
		if _, err := f.ReadAt(directory, directoryOffset); err != nil {
			return err
		}
		type span struct{ start, end int64 }
		spans := make([]span, 0, 2)
		position := 0
		for range 2 {
			if len(directory)-position < 46 {
				return errors.New("备份 ZIP 目录条目截断")
			}
			record := directory[position:]
			if binary.LittleEndian.Uint32(record) != 0x02014b50 || binary.LittleEndian.Uint16(record[34:]) != 0 {
				return errors.New("备份 ZIP 目录条目或磁盘无效")
			}
			compressed := binary.LittleEndian.Uint32(record[20:])
			uncompressed := binary.LittleEndian.Uint32(record[24:])
			localOffset := binary.LittleEndian.Uint32(record[42:])
			if compressed == 0xffffffff || uncompressed == 0xffffffff || localOffset == 0xffffffff {
				return errors.New("备份不支持 ZIP64")
			}
			nameSize := int(binary.LittleEndian.Uint16(record[28:]))
			extraSize := int(binary.LittleEndian.Uint16(record[30:]))
			commentSize := int(binary.LittleEndian.Uint16(record[32:]))
			recordSize := 46 + nameSize + extraSize + commentSize
			if recordSize > len(record) {
				return errors.New("备份 ZIP 目录条目长度无效")
			}
			name := record[46 : 46+nameSize]
			if string(name) != "manifest.json" && string(name) != "database.db" {
				return errors.New("备份包含不允许的归档路径")
			}
			if err := rejectZip64Extra(record[46+nameSize : 46+nameSize+extraSize]); err != nil {
				return err
			}
			start := int64(localOffset)
			if start+30 > directoryOffset {
				return errors.New("备份 ZIP 本地条目范围无效")
			}
			local := make([]byte, 30)
			if _, err := f.ReadAt(local, start); err != nil {
				return err
			}
			if binary.LittleEndian.Uint32(local) != 0x04034b50 || binary.LittleEndian.Uint16(local[6:]) != binary.LittleEndian.Uint16(record[8:]) || binary.LittleEndian.Uint16(local[8:]) != binary.LittleEndian.Uint16(record[10:]) {
				return errors.New("备份 ZIP 本地条目头无效")
			}
			localNameSize := int(binary.LittleEndian.Uint16(local[26:]))
			localExtraSize := int(binary.LittleEndian.Uint16(local[28:]))
			bodyOffset := start + 30 + int64(localNameSize+localExtraSize)
			if bodyOffset+int64(compressed) > directoryOffset || localNameSize != nameSize {
				return errors.New("备份 ZIP 本地条目范围无效")
			}
			localNames := make([]byte, localNameSize+localExtraSize)
			if _, err := f.ReadAt(localNames, start+30); err != nil {
				return err
			}
			if !bytes.Equal(localNames[:localNameSize], name) {
				return errors.New("备份 ZIP 本地路径不匹配")
			}
			if err := rejectZip64Extra(localNames[localNameSize:]); err != nil {
				return err
			}
			end := bodyOffset + int64(compressed)
			if binary.LittleEndian.Uint16(local[6:])&8 != 0 {
				// Classic data descriptors can have an optional four-byte signature.
				descriptor := make([]byte, 16)
				if end+12 > directoryOffset {
					return errors.New("备份 ZIP 数据描述符范围无效")
				}
				readSize := int64(16)
				if end+readSize > directoryOffset {
					readSize = 12
				}
				if _, err := f.ReadAt(descriptor[:readSize], end); err != nil {
					return err
				}
				signatureSize := 0
				if binary.LittleEndian.Uint32(descriptor) == 0x08074b50 {
					signatureSize = 4
				}
				if int(readSize) < signatureSize+12 {
					return errors.New("备份 ZIP 数据描述符截断")
				}
				descriptor = descriptor[signatureSize:]
				if binary.LittleEndian.Uint32(descriptor) != binary.LittleEndian.Uint32(record[16:]) || binary.LittleEndian.Uint32(descriptor[4:]) != compressed || binary.LittleEndian.Uint32(descriptor[8:]) != uncompressed {
					return errors.New("备份 ZIP 数据描述符不匹配")
				}
				end += int64(signatureSize + 12)
			} else if binary.LittleEndian.Uint32(local[14:]) != binary.LittleEndian.Uint32(record[16:]) || binary.LittleEndian.Uint32(local[18:]) != compressed || binary.LittleEndian.Uint32(local[22:]) != uncompressed {
				return errors.New("备份 ZIP 本地大小或校验和不匹配")
			}
			spans = append(spans, span{start: start, end: end})
			position += recordSize
		}
		if position != len(directory) {
			return errors.New("备份 ZIP 含额外目录条目")
		}
		sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
		if spans[0].start != 0 || spans[0].end != spans[1].start || spans[1].end != directoryOffset {
			return errors.New("备份 ZIP 含额外数据或重叠条目")
		}
		return nil
	}
	return errors.New("备份 ZIP 目录损坏")
}

func rejectZip64Extra(extra []byte) error {
	for len(extra) > 0 {
		if len(extra) < 4 {
			return errors.New("备份 ZIP 扩展字段截断")
		}
		kind := binary.LittleEndian.Uint16(extra)
		length := int(binary.LittleEndian.Uint16(extra[2:]))
		if kind == 1 {
			return errors.New("备份不支持 ZIP64")
		}
		if length > len(extra)-4 {
			return errors.New("备份 ZIP 扩展字段长度无效")
		}
		extra = extra[4+length:]
	}
	return nil
}

func inspectSnapshot(ctx context.Context, path string) (BackupManifest, error) {
	var result BackupManifest
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(contextReader{ctx: ctx, r: f}, backupExtractLimit+1))
	closeErr := f.Close()
	if err != nil {
		return result, err
	}
	if closeErr != nil {
		return result, closeErr
	}
	if n > backupExtractLimit {
		return result, errors.New("数据库快照超过 256 MiB 预算")
	}
	db, version, err := openMaintenanceSource(ctx, path)
	if err != nil {
		return result, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return result, err
	}
	ok := false
	for rows.Next() {
		var message string
		if err = rows.Scan(&message); err != nil {
			break
		}
		if message != "ok" {
			err = errors.New("备份数据库完整性检查失败")
			break
		}
		ok = true
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if rowErr != nil {
		return result, rowErr
	}
	if !ok {
		return result, errors.New("备份数据库完整性检查失败")
	}
	rows, err = db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return result, err
	}
	hasViolation := rows.Next()
	rowErr = rows.Err()
	rows.Close()
	if rowErr != nil {
		return result, rowErr
	}
	if hasViolation {
		return result, errors.New("备份数据库外键检查失败")
	}
	result.Counts = make(map[string]int64)
	tables := []string{"targets", "checks"}
	if version >= 2 {
		tables = append(tables, "daily_summaries", "monitor_metadata")
	}
	for _, table := range tables {
		var count int64
		if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			return result, fmt.Errorf("备份数据库数据计数失败: %w", err)
		}
		result.Counts[table] = count
	}
	result.SchemaVersion = version
	result.SnapshotSize = n
	result.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return result, nil
}

type budgetWriter struct {
	w    io.Writer
	left int64
}

func (w *budgetWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, errors.New("备份超过 256 MiB 压缩预算")
	}
	n, err := w.w.Write(p)
	w.left -= int64(n)
	return n, err
}
