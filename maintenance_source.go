package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// openMaintenanceSource never runs migrations, retention, or scheduling. mode=ro
// still permits SQLite's ordinary WAL shared-memory coordination when needed.
func openMaintenanceSource(ctx context.Context, path string) (*sql.DB, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, fmt.Errorf("读取维护源失败: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("维护源必须是现有普通数据库文件")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, 0, err
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	uri := url.URL{Scheme: "file", Path: p}
	q := uri.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(0)")
	q.Add("_pragma", "query_only(ON)")
	uri.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, 0, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, 0, fmt.Errorf("读取维护源失败: %w", err)
	}
	if version < 1 || version > 4 {
		db.Close()
		return nil, 0, fmt.Errorf("不支持数据库版本 %d（支持 1–4）", version)
	}
	return db, version, nil
}

func validateNewDatabasePath(dest string) error {
	if strings.TrimSpace(dest) == "" {
		return errors.New("新数据库路径不能为空")
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		_, err := os.Lstat(dest + suffix)
		if err == nil {
			return errors.New("目标数据库或 SQLite 侧车文件已经存在")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("检查目标数据库路径失败: %w", err)
		}
	}
	return nil
}

// A hard-link creates the destination atomically and fails if it already exists.
// Callers create their temporary file in the destination directory, so the link
// stays on one filesystem. A filesystem without hard links fails closed.
func publishNewFile(temp, dest string) error {
	if err := os.Link(temp, dest); err != nil {
		return fmt.Errorf("发布新文件失败（不会覆盖现有文件）: %w", err)
	}
	// The destination is complete now. A leftover private temporary name does not
	// invalidate that committed result; callers also defer temporary cleanup.
	_ = os.Remove(temp)
	return nil
}

func writeNewOutput(ctx context.Context, dest string, write func(io.Writer) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return errors.New("输出文件已经存在")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".pulse-output-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	defer f.Close()
	if err = write(contextWriter{ctx: ctx, w: f}); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return publishNewFile(name, dest)
}

type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
