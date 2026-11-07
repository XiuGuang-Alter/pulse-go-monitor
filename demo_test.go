package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDemoLifecycle(t *testing.T) {
	store := testStore(t)
	address, cleanup, err := startDemo(store, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	targets, err := store.ListTargets(context.Background())
	if err != nil || len(targets) != 3 {
		t.Fatalf("targets=%+v err=%v", targets, err)
	}
	for _, target := range targets {
		if !strings.HasPrefix(target.URL, "http://"+address+"/") {
			t.Fatalf("wrong demo URL: %s", target.URL)
		}
	}
	response, err := http.Get("http://" + address + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	if _, otherCleanup, err := startDemo(store, address); err == nil {
		otherCleanup()
		t.Fatal("occupied demo port accepted")
	}
	cleanup()
	if connection, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		connection.Close()
		t.Fatal("demo listener leaked")
	}
	_, again, err := startDemo(store, address)
	if err != nil {
		t.Fatal(err)
	}
	defer again()
	targets, err = store.ListTargets(context.Background())
	if err != nil || len(targets) != 3 {
		t.Fatalf("duplicate demo targets after restart: %d %v", len(targets), err)
	}
}

func TestServeRejectsNonLoopbackDemoAddress(t *testing.T) {
	var out bytes.Buffer
	if code := runServe(context.Background(), []string{"-addr", "127.0.0.1:0", "-demo", "-demo-addr", "0.0.0.0:0", "-db", filepath.Join(t.TempDir(), "db")}, &out, &out); code != 2 {
		t.Fatalf("exit=%d %s", code, &out)
	}
}

func TestDemoStartupFailureReleasesPort(t *testing.T) {
	store := testStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	if _, cleanup, err := startDemo(store, address); err == nil {
		cleanup()
		t.Fatal("closed database accepted")
	}
	listener, err = net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("startup failure leaked listener: %v", err)
	}
	listener.Close()
}
