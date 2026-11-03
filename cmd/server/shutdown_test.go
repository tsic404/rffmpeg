package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type recordingShutdown struct {
	name string
	log  *[]string
	err  error
}

func (r *recordingShutdown) Shutdown(context.Context) error {
	*r.log = append(*r.log, r.name)
	return r.err
}

func (r *recordingShutdown) Close() { *r.log = append(*r.log, r.name) }
func (r *recordingShutdown) Stop()  { *r.log = append(*r.log, r.name) }

func TestShutdownServerStopsHTTPBeforeScheduling(t *testing.T) {
	var order []string
	srv := &recordingShutdown{name: "http", log: &order}
	hub := &recordingShutdown{name: "websocket", log: &order}
	scheduler := &recordingShutdown{name: "scheduler", log: &order}
	monitor := &recordingShutdown{name: "monitor", log: &order}

	if err := shutdownServer(context.Background(), srv, hub, scheduler, monitor); err != nil {
		t.Fatalf("shutdownServer: %v", err)
	}

	want := []string{"http", "websocket", "scheduler", "monitor"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("shutdown order = %v, want %v", order, want)
	}
}

func TestShutdownServerContinuesAfterHTTPTimeout(t *testing.T) {
	var order []string
	drainErr := errors.New("context deadline exceeded")
	srv := &recordingShutdown{name: "http", log: &order, err: drainErr}
	hub := &recordingShutdown{name: "websocket", log: &order}
	scheduler := &recordingShutdown{name: "scheduler", log: &order}
	monitor := &recordingShutdown{name: "monitor", log: &order}

	err := shutdownServer(context.Background(), srv, hub, scheduler, monitor)
	if !errors.Is(err, drainErr) {
		t.Fatalf("error = %v, want the HTTP drain error", err)
	}
	if len(order) != 4 {
		t.Fatalf("teardown stopped after HTTP error: order = %v", order)
	}
}
