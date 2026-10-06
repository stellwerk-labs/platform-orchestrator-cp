package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRunEveryRepeatsBoundedRunsLogsFailuresAndStops(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	ctx, cancel := context.WithCancel(t.Context())
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		runEvery(ctx, 5*time.Millisecond, time.Second, zap.New(core), "retention", func(run context.Context) error {
			_, bounded := run.Deadline()
			assert.True(t, bounded, "every run gets its own deadline")
			if calls.Add(1) == 3 {
				cancel()
			}
			return errors.New("database unavailable")
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runEvery did not stop after cancellation")
	}
	require.Equal(t, int32(3), calls.Load(), "a failed run is retried on the next tick")
	entries := logs.FilterMessage("Plugin maintenance job failed").All()
	require.Len(t, entries, 2, "a failure after cancellation is shutdown, not an error")
	require.Equal(t, "retention", entries[0].ContextMap()["job"])
}
