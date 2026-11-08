package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceHelpAndUnknownCommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"maintain", "help"}, 0, "export-history"},
		{[]string{"maintain", "nonsense"}, 2, "维护命令"},
		{[]string{"maintain", "backup"}, 2, "-db"},
	} {
		var out, errOut bytes.Buffer
		code := run(tc.args, &out, &errOut)
		if code != tc.code || !strings.Contains(out.String()+errOut.String(), tc.want) {
			t.Fatalf("args=%v code=%d out=%s err=%s", tc.args, code, &out, &errOut)
		}
	}
}

func TestMaintenanceRejectsInvalidRangeBeforeAccessingFiles(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"maintain", "export-history", "-db", "missing.db", "-out", "unused.json", "-target", "1", "-from", "2026-11-01T00:00:00Z", "-to", "2026-11-01T00:00:00Z"}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "历史参数无效") {
		t.Fatalf("code=%d out=%s err=%s", code, &out, &errOut)
	}
}

func TestMaintenanceCancellationAndErrorsDoNotLeakPaths(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "export.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	code := runMaintenance(ctx, []string{"export-config", "-db", filepath.Join(dir, "missing-token-fake-cli-secret.db"), "-out", output}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "CANCELED") {
		t.Fatalf("code=%d err=%s", code, &errOut)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("cancelled output exists: %v", err)
	}
	errOut.Reset()
	code = runMaintenance(context.Background(), []string{"export-config", "-db", filepath.Join(dir, "missing-token-fake-cli-secret.db"), "-out", output}, &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "NOT_FOUND") || strings.Contains(errOut.String(), "fake-cli-secret") {
		t.Fatalf("code=%d unsafe error=%s", code, &errOut)
	}
}
