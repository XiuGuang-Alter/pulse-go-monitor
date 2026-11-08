package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestMaintenanceSourceMissingDoesNotCreate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing.db")
	if db, _, err := openMaintenanceSource(context.Background(), p); err == nil {
		db.Close()
		t.Fatal("missing source accepted")
	}
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("source created", err)
	}
}

func TestMaintenanceNewPathAndOutput(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "new.db")
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		p := dest + suffix
		if err := os.WriteFile(p, []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := validateNewDatabasePath(dest); err == nil {
			t.Fatal("accepted existing", suffix)
		}
		os.Remove(p)
	}
	injected := errors.New("injected write failure")
	if err := writeNewOutput(context.Background(), dest, func(w io.Writer) error { w.Write([]byte("partial")); return injected }); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial output published", err)
	}
	if err := writeNewOutput(context.Background(), dest, func(w io.Writer) error { _, err := io.WriteString(w, "first"); return err }); err != nil {
		t.Fatal(err)
	}
	if err := writeNewOutput(context.Background(), dest, func(w io.Writer) error { _, err := io.WriteString(w, "second"); return err }); err == nil {
		t.Fatal("overwrote output")
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "first" {
		t.Fatal(string(b))
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary artifacts", entries)
	}
}

func TestMaintenancePublishNoClobber(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "dest")
	temp := filepath.Join(dir, "temp")
	os.WriteFile(dest, []byte("old"), 0600)
	os.WriteFile(temp, []byte("new"), 0600)
	if err := publishNewFile(temp, dest); err == nil {
		t.Fatal("overwrote destination")
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "old" {
		t.Fatal(string(b))
	}
}

func TestMaintenanceSourceIsReadOnlyAndDoesNotMigrate(t *testing.T) {
	source, writer := backupFixture(t, 1, false)
	db, version, err := openMaintenanceSource(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if version != 1 {
		t.Fatal("schema migrated", version)
	}
	if _, err = db.Exec("DELETE FROM checks"); err == nil {
		t.Fatal("maintenance source permits writes")
	}
	var n int
	if err = writer.QueryRow("SELECT COUNT(*) FROM checks").Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, _, err = openMaintenanceSource(context.Background(), t.TempDir()); err == nil {
		t.Fatal("directory source accepted")
	}
}

func TestMaintenanceOutputSyncFailureCancelAndPublishRace(t *testing.T) {
	for _, kind := range []string{"sync", "cancel", "publish"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "output")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := writeNewOutput(ctx, dest, func(w io.Writer) error {
				if _, err := io.WriteString(w, "partial"); err != nil {
					return err
				}
				switch kind {
				case "sync":
					return w.(contextWriter).w.(*os.File).Close()
				case "cancel":
					cancel()
				case "publish":
					return os.WriteFile(dest, []byte("race winner"), 0600)
				}
				return nil
			})
			if err == nil {
				t.Fatal("injected failure succeeded")
			}
			if kind == "publish" {
				b, _ := os.ReadFile(dest)
				if string(b) != "race winner" {
					t.Fatal("race clobbered", string(b))
				}
			} else if _, err = os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed output published", err)
			}
			entries, _ := os.ReadDir(dir)
			want := 0
			if kind == "publish" {
				want = 1
			}
			if len(entries) != want {
				t.Fatal("temporary output left", entries)
			}
		})
	}
}

func TestMaintenanceRejectsSymlinksIncludingDanglingSidecars(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "new.db")
	missing := filepath.Join(dir, "missing")
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		link := dest + suffix
		if err := os.Symlink(missing, link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if err := validateNewDatabasePath(dest); err == nil {
			t.Fatal("dangling symlink accepted", suffix)
		}
		if suffix == "" {
			if db, _, err := openMaintenanceSource(context.Background(), link); err == nil {
				db.Close()
				t.Fatal("source symlink accepted")
			}
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
}
