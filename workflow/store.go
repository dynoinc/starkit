package workflow

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dynoinc/starflow/workflow/events"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store provides a persistent storage for workflow traces and scripts.
type Store struct {
	p *pgxpool.Pool
	q *Queries

	hook func(runID string, version int, event events.Event)
}

type StoreOption func(*Store)

func WithHook(hook func(runID string, version int, event events.Event)) StoreOption {
	return func(s *Store) {
		s.hook = hook
	}
}

// NewStore creates a new Store and runs database migrations.
func NewStore(ctx context.Context, dsn string, opts ...StoreOption) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	if err := Migrate(ctx, pool); err != nil {
		return nil, fmt.Errorf("failed to migrate db: %w", err)
	}

	s := &Store{
		p: pool,
		q: New(pool),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Migrate runs the database migrations.
func Migrate(ctx context.Context, p *pgxpool.Pool) error {
	migrationDirEntries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("failed to read migration files: %w", err)
	}

	for _, file := range migrationDirEntries {
		if !strings.HasSuffix(file.Name(), ".sql") {
			continue
		}

		migrationContent, err := migrationFiles.ReadFile("migrations/" + file.Name())
		if err != nil {
			return fmt.Errorf("failed to read migration file %s: %w", file.Name(), err)
		}

		if _, err := p.Exec(ctx, string(migrationContent)); err != nil {
			return fmt.Errorf("failed to apply migration %s: %w", file.Name(), err)
		}
	}
	return nil
}

func (s *Store) Close() {
	s.p.Close()
}

// AppendEvent appends an event to a run's history.
func (s *Store) AppendEvent(ctx context.Context, runID string, expectedVersion int, eventData []byte) (int, error) {
	if s.hook != nil {
		var event events.Event
		if err := json.Unmarshal(eventData, &event); err != nil {
			return 0, fmt.Errorf("failed to unmarshal event: %w", err)
		}

		s.hook(runID, expectedVersion, event)
	}

	tx, err := s.p.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)

	currentVersion, err := qtx.GetRunVersionForUpdate(ctx, runID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("failed to get run version: %w", err)
		}
		// If no rows, this is a new run.
		currentVersion = 0
	}

	if int(currentVersion) != expectedVersion {
		return 0, ErrConcurrentUpdate
	}

	newVersion := expectedVersion + 1

	if _, err := qtx.UpsertRun(ctx, UpsertRunParams{
		ID:      runID,
		Version: int32(newVersion),
	}); err != nil {
		return 0, fmt.Errorf("failed to upsert run: %w", err)
	}

	if err := qtx.InsertEvent(ctx, InsertEventParams{
		RunID:   runID,
		Version: int32(newVersion),
		Data:    eventData,
	}); err != nil {
		return 0, fmt.Errorf("failed to insert event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return newVersion, nil
}

// GetEvents returns the event data for a given run in the order they were recorded.
func (s *Store) GetEvents(ctx context.Context, runID string) ([][]byte, error) {
	return s.q.GetEvents(ctx, runID)
}

// GetLastEvent returns the last event data and version for a given run.
func (s *Store) GetLastEvent(ctx context.Context, runID string) ([]byte, int, error) {
	event, err := s.q.GetLastEvent(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	return event.Data, int(event.Version), nil
}

// GetFirstEvent returns the first event data for a given run.
func (s *Store) GetFirstEvent(ctx context.Context, runID string) ([]byte, error) {
	data, err := s.q.GetFirstEvent(ctx, runID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// GetScript returns the script data for a given script hash.
func (s *Store) GetScript(ctx context.Context, scriptHash string) ([]byte, error) {
	data, err := s.q.GetScript(ctx, scriptHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// PutScript stores the script data for a given script hash.
func (s *Store) PutScript(ctx context.Context, scriptHash string, scriptData []byte) error {
	return s.q.PutScript(ctx, PutScriptParams{
		Hash: scriptHash,
		Data: scriptData,
	})
}
