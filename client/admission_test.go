package client

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func admissionClient(t *testing.T, active, queued int) *Client {
	t.Helper()
	c, err := New(Config{Host: "unused.invalid", Username: "reader", Password: "secret", MaxConcurrentReads: active, MaxQueuedReads: queued, QueueTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAdmissionBurstIsBoundedAndCancellationReclaimsQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := admissionClient(t, 2, 3)
		for range 2 {
			if err := c.acquire(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		results := make(chan error, 50)
		for range 50 {
			go func() { results <- c.acquire(ctx) }()
		}
		synctest.Wait()
		// Exactly three calls wait; the rest reject immediately without consuming
		// an active slot. This assertion also detects accidentally unbounded queues.
		if len(results) != 47 || len(c.waiters) != 3 || len(c.slots) != 2 {
			t.Fatalf("results=%d queued=%d active=%d", len(results), len(c.waiters), len(c.slots))
		}
		for range 47 {
			if err := <-results; !errors.Is(err, ErrBusy) {
				t.Fatal(err)
			}
		}
		cancel()
		synctest.Wait()
		for range 3 {
			if err := <-results; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		}
		if len(c.waiters) != 0 {
			t.Fatal("canceled calls retained queue places")
		}
		// Queue places are reusable; releasing active admission wakes the new waiter.
		go func() { results <- c.acquire(context.Background()) }()
		synctest.Wait()
		<-c.slots
		synctest.Wait()
		if err := <-results; err != nil {
			t.Fatal(err)
		}
		if len(c.waiters) != 0 || len(c.slots) != 2 {
			t.Fatal("admission leaked or exceeded active limit")
		}
		<-c.slots
		<-c.slots
	})
}

func TestAdmissionWaitTimeoutAndParentDeadline(t *testing.T) {
	for _, parentFirst := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			c := admissionClient(t, 1, 1)
			if err := c.acquire(context.Background()); err != nil {
				t.Fatal(err)
			}
			duration, want := 2*time.Second, ErrBusy
			if parentFirst {
				duration, want = 100*time.Millisecond, context.DeadlineExceeded
			}
			ctx, cancel := context.WithTimeout(context.Background(), duration)
			defer cancel()
			start := time.Now()
			err := c.acquire(ctx)
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			expected := time.Second
			if parentFirst {
				expected = duration
			}
			if time.Since(start) != expected {
				t.Fatalf("wait=%v want %v", time.Since(start), expected)
			}
			if len(c.waiters) != 0 || len(c.slots) != 1 {
				t.Fatal("timeout leaked admission")
			}
			canceled, stop := context.WithCancel(context.Background())
			stop()
			if err := c.acquire(canceled); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			<-c.slots
		})
	}
}

func TestAdmissionQueueDeadlineDoesNotExpireAdmittedOperation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := admissionClient(t, 1, 1)
		if err := c.acquire(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- c.acquire(ctx) }()
		synctest.Wait()
		time.Sleep(500 * time.Millisecond)
		<-c.slots
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		if ctx.Err() != nil || len(c.slots) != 1 || len(c.waiters) != 0 {
			t.Fatal("queue deadline affected admitted operation")
		}
		<-c.slots
	})
}
