package workflow_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/dynoinc/starflow/workflow"
)

// setupTestDB sets up a test database using testcontainers.
func setupTestDB(t *testing.T) *workflow.Store {
	t.Helper()

	ctx := t.Context()
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	postgresContainer, err := postgres.Run(ctx, "postgres:16-alpine", postgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { _ = postgresContainer.Terminate(t.Context()) })

	store, err := workflow.NewStore(ctx, postgresContainer.MustConnectionString(ctx, "sslmode=disable"))
	require.NoError(t, err)
	t.Cleanup(store.Close)
	return store
}

func TestStore_AppendAndGet(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()
	runID := "test-run"

	// Append first event
	_, err := store.AppendEvent(ctx, runID, 0, []byte("event1"))
	require.NoError(t, err)

	// Append second event
	_, err = store.AppendEvent(ctx, runID, 1, []byte("event2"))
	require.NoError(t, err)

	// Get all events
	events, err := store.GetEvents(ctx, runID)
	require.NoError(t, err)
	assert.Len(t, events, 2)
	assert.Equal(t, []byte("event1"), events[0])
	assert.Equal(t, []byte("event2"), events[1])

	// Get last event
	lastEvent, version, err := store.GetLastEvent(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, []byte("event2"), lastEvent)
	assert.Equal(t, 2, version)

	// Get first event
	firstEvent, err := store.GetFirstEvent(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, []byte("event1"), firstEvent)
}

func TestStore_ConcurrentAppend(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()
	runID := "concurrent-run"

	// Initial event, version becomes 1
	_, err := store.AppendEvent(ctx, runID, 0, []byte("event0"))
	require.NoError(t, err)

	numGoroutines := 4
	var wg sync.WaitGroup
	successCh := make(chan bool, numGoroutines)

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			// All goroutines try to append with expectedVersion = 1
			_, err := store.AppendEvent(ctx, runID, 1, []byte("concurrent-event"))
			successCh <- (err == nil)
		}()
	}
	wg.Wait()
	close(successCh)

	successCount := 0
	for success := range successCh {
		if success {
			successCount++
		}
	}

	// Only one should succeed
	require.Equal(t, 1, successCount, "expected exactly one concurrent append to succeed")

	// Verify the final state
	events, err := store.GetEvents(ctx, runID)
	require.NoError(t, err)
	assert.Len(t, events, 2) // event0 + one concurrent-event

	_, version, err := store.GetLastEvent(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, 2, version)
}

func TestStore_Scripts(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()
	scriptHash := "test-script-hash"
	scriptData := []byte("print('hello world')")

	// Put script
	err := store.PutScript(ctx, scriptHash, scriptData)
	require.NoError(t, err)

	// Get script
	retrievedData, err := store.GetScript(ctx, scriptHash)
	require.NoError(t, err)
	assert.Equal(t, scriptData, retrievedData)

	// Get non-existent script
	retrievedData, err = store.GetScript(ctx, "non-existent-hash")
	require.NoError(t, err)
	assert.Nil(t, retrievedData)
}

func TestStore_NewRun(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	// Append event to a new run
	_, err := store.AppendEvent(ctx, "new-run", 0, []byte("first"))
	require.NoError(t, err)

	// Try to append again with version 0 (should fail)
	_, err = store.AppendEvent(ctx, "new-run", 0, []byte("should-fail"))
	assert.ErrorIs(t, err, workflow.ErrConcurrentUpdate)
}

func TestStore_EmptyRun(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	events, err := store.GetEvents(ctx, "empty-run")
	require.NoError(t, err)
	assert.Len(t, events, 0)

	last, v, err := store.GetLastEvent(ctx, "empty-run")
	require.NoError(t, err)
	assert.Nil(t, last)
	assert.Equal(t, 0, v)

	first, err := store.GetFirstEvent(ctx, "empty-run")
	require.NoError(t, err)
	assert.Nil(t, first)
}
