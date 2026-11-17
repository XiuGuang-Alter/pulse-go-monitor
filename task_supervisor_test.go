package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestTaskSupervisorRestartsOnlyFailedWorker(t *testing.T) {
	failure := errors.New("worker failed")
	for _, tc := range []struct {
		name    string
		results []error
		want    int
		wantErr error
	}{
		{"normal exit", []error{nil}, 1, nil},
		{"failure and recovery", []error{failure, nil}, 2, nil},
		{"retry exhaustion", []error{failure, failure, failure, failure, nil}, 4, failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := superviseTask(context.Background(), time.Millisecond, io.Discard, func(context.Context) error {
				result := tc.results[calls]
				calls++
				return result
			})
			if calls != tc.want || !errors.Is(err, tc.wantErr) {
				t.Fatalf("calls = %d, error = %v", calls, err)
			}
		})
	}
}

func TestTaskSupervisorStopCancelsPendingRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	calls := 0
	go func() {
		done <- superviseTask(ctx, time.Minute, io.Discard, func(context.Context) error {
			calls++
			if calls == 1 {
				close(started)
			}
			return errors.New("worker failed")
		})
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil || calls != 1 {
			t.Fatalf("stop returned %v after %d worker starts", err, calls)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not cancel the pending retry")
	}
}
