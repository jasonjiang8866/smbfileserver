package client

import (
	"context"
	"testing"
	"testing/synctest"
)

func TestStatsReflectAdmissionAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := admissionClient(t, 1, 1)
		if got := c.Stats(); got != (Stats{}) {
			t.Fatal(got)
		}
		if err := c.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- c.acquire(ctx) }()
		synctest.Wait()
		if got := c.Stats(); got != (Stats{ActiveReads: 1, QueuedReads: 1}) {
			t.Fatal(got)
		}
		cancel()
		synctest.Wait()
		<-done
		<-c.slots
		if got := c.Stats(); got != (Stats{}) {
			t.Fatal("admission leaked", got)
		}
	})
}
