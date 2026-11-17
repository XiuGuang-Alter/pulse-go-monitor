package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsTaskRejectsInvalidStopTokenBeforeDatabase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "do-not-create.db")
	var output bytes.Buffer
	code := runServe(context.Background(), []string{"-db", db, "-windows-stop-event", `Global\not-an-event`}, &output, &output)
	if code != 2 || !strings.Contains(output.String(), "停止标识") {
		t.Fatalf("code = %d, output = %q", code, output.String())
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("invalid task options touched database: %v", err)
	}
}
