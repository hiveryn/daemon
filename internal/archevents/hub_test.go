package archevents

import (
	"testing"

	"github.com/hiveryn/daemon/internal/domain"
)

func TestHubDeliversToSubscriber(t *testing.T) {
	hub := New(nil)
	sub := hub.Subscribe("alpha")
	defer sub.Close()

	hub.Publish("alpha", domain.ArchitectEvent{Type: "workspace_changed", TicketID: "t1"})

	select {
	case event, ok := <-sub.C():
		if !ok {
			t.Fatal("channel closed before delivery")
		}
		if event.TicketID != "t1" {
			t.Fatalf("got ticket %q, want t1", event.TicketID)
		}
	default:
		t.Fatal("event was not delivered")
	}
}

func TestHubIsolatesKeys(t *testing.T) {
	hub := New(nil)
	sub := hub.Subscribe("alpha")
	defer sub.Close()

	hub.Publish("beta", domain.ArchitectEvent{TicketID: "t1"})

	select {
	case <-sub.C():
		t.Fatal("received event published to a different key")
	default:
	}
}

func TestHubClosesOverflowedSubscriber(t *testing.T) {
	hub := New(nil)
	sub := hub.Subscribe("alpha")
	defer sub.Close()

	// Fill the buffer plus one. A subscriber that never drains overflows and is
	// closed rather than silently dropping the event.
	for i := 0; i < subscriberBuffer+1; i++ {
		hub.Publish("alpha", domain.ArchitectEvent{TicketID: "t1"})
	}

	// Drain everything; the channel must end up closed (ok == false).
	closed := false
	for !closed {
		if _, ok := <-sub.C(); !ok {
			closed = true
		}
	}
}

func TestHubCloseAfterOverflowDoesNotPanic(t *testing.T) {
	hub := New(nil)
	sub := hub.Subscribe("alpha")

	for i := 0; i < subscriberBuffer+1; i++ {
		hub.Publish("alpha", domain.ArchitectEvent{TicketID: "t1"})
	}

	// The overflow path already closed the channel; the handler's deferred Close
	// must be a safe no-op, not a double-close panic.
	sub.Close()
	sub.Close()
}

func TestHubShutdownClosesSubscribers(t *testing.T) {
	hub := New(nil)
	sub := hub.Subscribe("alpha")
	defer sub.Close()

	hub.Shutdown()

	if _, ok := <-sub.C(); ok {
		t.Fatal("expected channel closed after shutdown")
	}
}
