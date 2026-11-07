package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The helper gets a private hidden console on Windows. CTRL_C_EVENT is never
// sent from the test runner's (or the user's) shared console.
func TestConsoleSignalHelper(t *testing.T) {
	if os.Getenv("PULSE_SIGNAL_HELPER") != "1" {
		return
	}
	if err := prepareConsole(); err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	command := exec.Command(os.Getenv("PULSE_SIGNAL_EXE"), "serve", "-addr", "127.0.0.1:0", "-db", os.Getenv("PULSE_SIGNAL_DB"))
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	fmt.Printf("CHILD %d\n", command.Process.Pid)
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() {
		t.Fatal("server failed before announcement")
	}
	_, address, ok := strings.Cut(scanner.Text(), "http://")
	if !ok {
		t.Fatalf("invalid announcement: %s", scanner.Text())
	}
	fmt.Printf("READY http://%s\n", address)
	go func() {
		for scanner.Scan() {
		}
	}()
	if _, err = bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err = interruptChild(command.Process); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signals:
		fmt.Fprintln(os.Stderr, "helper received interrupt")
	case <-time.After(time.Second):
		fmt.Fprintln(os.Stderr, "helper did not receive broadcast interrupt")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child ignored interrupt")
	}
}

func TestRealProcessInterrupt(t *testing.T) {
	directory := t.TempDir()
	exe := filepath.Join(directory, "pulse")
	goExe := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		exe += ".exe"
		goExe += ".exe"
	}
	build := exec.Command(goExe, "build", "-buildvcs=false", "-o", exe, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
			canceled <- struct{}{}
		case <-unblock:
		}
	}))
	defer upstream.Close()
	defer close(unblock)
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(directory, "monitor.db")
	helper := exec.Command(testExe, "-test.run=^TestConsoleSignalHelper$")
	helper.Env = append(os.Environ(), "PULSE_SIGNAL_HELPER=1", "PULSE_SIGNAL_EXE="+exe, "PULSE_SIGNAL_DB="+dbPath)
	isolateConsole(helper)
	output, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	input, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	helper.Stderr = &stderr
	if err = helper.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- helper.Wait() }()
	var child *os.Process
	defer func() {
		if child != nil {
			child.Kill()
			child.Release()
		}
		helper.Process.Kill()
	}()
	ready := make(chan string, 1)
	childID := make(chan int, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "CHILD ") {
				var id int
				fmt.Sscanf(line, "CHILD %d", &id)
				childID <- id
			}
			if strings.HasPrefix(line, "READY ") {
				ready <- strings.TrimPrefix(line, "READY ")
			}
		}
	}()
	var base string
	select {
	case id := <-childID:
		child, _ = os.FindProcess(id)
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not start child")
	}
	select {
	case base = <-ready:
	case err = <-done:
		t.Fatalf("helper exited: %v %s", err, &stderr)
	case <-time.After(5 * time.Second):
		t.Fatal("child did not start")
	}
	client := &http.Client{Timeout: 3 * time.Second}
	inputTarget := TargetInput{Name: "signal test", URL: upstream.URL, IntervalSeconds: 60, TimeoutSeconds: 30, Enabled: true}
	body, _ := json.Marshal(inputTarget)
	response, err := client.Post(base+"/api/targets", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatal(response.StatusCode)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not reached")
	}
	if _, err = input.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("interrupt exit: %v %s", err, &stderr)
		}
	case <-time.After(12 * time.Second):
		helper.Process.Kill()
		<-done
		t.Fatalf("interrupt did not stop process: %s", &stderr)
	}
	// The helper verified that the child exited; do not later signal a recycled PID.
	child.Release()
	child = nil
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request not canceled")
	}
	if connection, err := net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), time.Second); err == nil {
		connection.Close()
		t.Fatal("listener leaked")
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	targets, err := store.ListTargets(context.Background())
	if err != nil || len(targets) != 1 || !sameTargetInput(targets[0].TargetInput, inputTarget) || targets[0].Stats.Samples != 0 {
		t.Fatalf("persistence/cancel: %+v %v", targets, err)
	}
}
