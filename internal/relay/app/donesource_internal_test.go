package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/apikdech/zrelay-v2/internal/relay/events"
)

// funcSource is an events.Source made of a function.
type funcSource func(ctx context.Context) error

func (f funcSource) Run(ctx context.Context, _ func(events.Signal), _ func(bool)) error {
	return f(ctx)
}

// TestDoneSourceClosesWhenTheSourceIsNotRunning: the supervisor's event loop
// sleeps in its backoff after a source returned early and never calls Run
// again once the camera context ends. Shutdown must not wait for a Run that
// will not come.
func TestDoneSourceClosesWhenTheSourceIsNotRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	src := newDoneSource(ctx, funcSource(func(context.Context) error { return errors.New("stopped early") }), done)

	// The supervisor ran it once, it returned, and the loop is now backing off.
	if err := src.Run(ctx, nil, nil); err == nil {
		t.Fatal("the source should have returned its error")
	}
	select {
	case <-done:
		t.Fatal("done closed before the camera context ended")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("done not closed after the camera context ended while the source was not running")
	}
}

// TestDoneSourceWaitsForARunningSourceToUnsubscribe: while Run is still
// returning (the ONVIF unsubscribe), done stays open until it has returned.
func TestDoneSourceWaitsForARunningSourceToUnsubscribe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	running := make(chan struct{})
	release := make(chan struct{})
	src := newDoneSource(ctx, funcSource(func(ctx context.Context) error {
		close(running)
		<-ctx.Done()
		<-release // the unsubscribe request
		return ctx.Err()
	}), make(chan struct{}))

	returned := make(chan struct{})
	go func() { _ = src.Run(ctx, nil, nil); close(returned) }()
	<-running
	cancel()
	select {
	case <-src.done:
		t.Fatal("done closed while the source was still unsubscribing")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-src.done:
	case <-time.After(time.Second):
		t.Fatal("done not closed after the source returned")
	}
	<-returned
}
