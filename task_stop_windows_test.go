package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsTaskStopEventGracefullyClosesServe(t *testing.T) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	token := hex.EncodeToString(nonce[:])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := filepath.Join(t.TempDir(), "isolation.db")
	ready := make(serveAnnouncement, 1)
	var output bytes.Buffer
	done := make(chan struct{})
	var exitCode int
	go func() {
		exitCode = runServe(ctx, []string{"-addr", "127.0.0.1:0", "-db", db, "-windows-stop-event", token}, ready, &output)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("server did not exit after context cancellation")
		}
	})
	var url string
	select {
	case url = <-ready:
	case <-done:
		t.Fatalf("server exited before readiness: %d %s", exitCode, &output)
	case <-time.After(8 * time.Second):
		t.Fatal("server did not start")
	}
	name := windows.StringToUTF16Ptr(`Local\PulseStop.` + token)
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var duplicate bytes.Buffer
	code := runServe(ctx, []string{"-addr", "127.0.0.1:0", "-db", filepath.Join(t.TempDir(), "other.db"), "-windows-stop-event", token}, &duplicate, &duplicate)
	if code != 1 || !strings.Contains(duplicate.String(), "已存在") {
		t.Fatalf("duplicate event accepted: %d %q", code, duplicate.String())
	}
	if err := windows.SetEvent(h); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if exitCode != 0 {
			t.Fatalf("expected normal exit: %d %s", exitCode, &output)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("server did not exit after stop event")
	}
	if conn, err := net.DialTimeout("tcp", strings.TrimPrefix(url, "http://"), time.Second); err == nil {
		conn.Close()
		t.Fatal("server listener remained open")
	}
	store, err := OpenStore(db)
	if err != nil {
		t.Fatalf("reopen database after stop: %v", err)
	}
	defer store.Close()
}
