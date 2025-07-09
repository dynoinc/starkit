// Package starflow provides a workflow engine for Go that enables deterministic
// and resumable workflow execution using Starlark scripting. Every
// execution step is recorded and can be resumed exactly where it left off.
//
// # Key Features
//
//   - Deterministic & Durable Workflows: Write workflows that are deterministic
//     and can be replayed from any point with full durability guarantees
//   - Pluggable Backends: Works with any backend that implements the Store interface
//   - Resumable Workflows: Workflows can yield and resume based on external signals
//
// For more information, see https://github.com/dynoinc/starflow
package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"runtime"
	"strings"
	"time"

	"go.starlark.net/syntax"

	"github.com/cenkalti/backoff/v4"

	"github.com/dynoinc/starflow/workflow/events"
)

type registeredFn struct {
	fn          func(ctx context.Context, req any) (any, error)
	name        string
	retryPolicy backoff.BackOff
}

// FunctionOption configures behaviour of a registered function.
type FunctionOption func(*registeredFn)

// WithName overrides the automatically derived name for the function.
// The name must be in the format "module.funcname".
func WithName(name string) FunctionOption {
	return func(rf *registeredFn) {
		// Validate that the name is in the correct format
		if !strings.Contains(name, ".") {
			panic(fmt.Sprintf("function name must be in format 'module.funcname', got: %s", name))
		}
		rf.name = name
	}
}

// WithRetryPolicy specifies a retry policy for the function.
func WithRetryPolicy(b backoff.BackOff) FunctionOption {
	return func(rf *registeredFn) {
		rf.retryPolicy = b
	}
}

// Client provides an interface for creating and managing workflow runs.
type Client[Input any, Output any] struct {
	store *Store

	registry map[string]registeredFn
}

// NewClient creates a new workflow client with the specified input type.
// The client uses the provided store for persistence and workflow management.
func NewClient[Input any, Output any](store *Store) *Client[Input, Output] {
	return &Client[Input, Output]{
		store:    store,
		registry: make(map[string]registeredFn),
	}
}

// RegisterFunc registers a Go function to be callable from Starlark using generics and reflection.
// The function must have the signature: func(ctx context.Context, req ReqType) (ResType, error)
// where ReqType and ResType are JSON-serializable types.
//
// The function will be automatically named based on its package and function name,
// or you can override this using the WithName option.
func RegisterFunc[Input any, Output any, Req any, Res any](
	c *Client[Input, Output],
	fn func(ctx context.Context, req Req) (Res, error),
	opts ...FunctionOption,
) {
	// Wrap the typed function to work with any
	wrappedFn := func(ctx context.Context, reqData any) (any, error) {
		var typedReq Req

		if reqData != nil {
			// Convert any to typed request via JSON marshaling
			reqBytes, err := json.Marshal(reqData)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal request: %w", err)
			}

			if err := json.Unmarshal(reqBytes, &typedReq); err != nil {
				return nil, fmt.Errorf("failed to unmarshal request: %w", err)
			}
		}
		// If reqData is nil, typedReq will be the zero value

		// Call the actual function
		res, err := fn(ctx, typedReq)
		if err != nil {
			return nil, err
		}

		// Return response as-is (Go will convert to any)
		return res, nil
	}

	reg := registeredFn{
		fn: wrappedFn,
	}
	for _, o := range opts {
		o(&reg)
	}

	if reg.name == "" {
		full := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
		function := strings.TrimPrefix(path.Ext(full), ".")
		module := path.Base(strings.TrimSuffix(full, path.Ext(full)))
		reg.name = module + "." + function
	}

	c.registry[reg.name] = reg
}

// ValidateScript performs validation on the Starlark script.
// It checks for syntax errors and ensures the script has a main function.
func ValidateScript(script []byte) error {
	// Parse the script to check for syntax errors
	parsed, err := (&syntax.FileOptions{}).Parse("script", script, 0)
	if err != nil {
		return fmt.Errorf("script syntax error: %w", err)
	}

	for _, stmt := range parsed.Stmts {
		if defStmt, ok := stmt.(*syntax.DefStmt); ok {
			if defStmt.Name.Name == "main" {
				return nil
			}
		}
	}

	return fmt.Errorf("script must contain a main function")
}

// Run creates a new workflow run with a script, and input, returning the run ID.
func (c *Client[Input, Output]) Run(ctx context.Context, runID string, script []byte, input Input) (Output, error) {
	var zero Output
	if err := ValidateScript(script); err != nil {
		return zero, fmt.Errorf("failed to validate script: %w", err)
	}

	h := sha256.New()
	h.Write(script)
	scriptHash := hex.EncodeToString(h.Sum(nil))
	if err := c.store.PutScript(ctx, scriptHash, script); err != nil {
		return zero, fmt.Errorf("failed to store script: %w", err)
	}

	return runThread(ctx, c, runID, scriptHash, script, input)
}

// GetEvents retrieves the execution history of a workflow run.
// Returns a chronological list of events that occurred during execution.
func (c *Client[Input, Output]) GetEvents(ctx context.Context, runID string) ([]*events.Event, error) {
	eventDataList, err := c.store.GetEvents(ctx, runID)
	if err != nil {
		return nil, err
	}

	// Convert byte slices back to Event structs
	evnts := make([]*events.Event, len(eventDataList))
	for i, eventData := range eventDataList {
		var event events.Event
		if err := json.Unmarshal(eventData, &event); err != nil {
			return nil, fmt.Errorf("failed to unmarshal event %d: %w", i, err)
		}
		evnts[i] = &event
	}

	return evnts, nil
}

// YieldError is returned when the script yields waiting for a signal.
type YieldError struct {
	runID string
}

func (e *YieldError) Error() string {
	return fmt.Sprintf("yield error (run: %s)", e.runID)
}

func NewYieldError(ctx context.Context) (string, error) {
	runID, ok := GetRunID(ctx)
	if !ok {
		return "", fmt.Errorf("runID not found in context")
	}

	return runID, &YieldError{runID: runID}
}

func (y *YieldError) Is(target error) bool {
	_, ok := target.(*YieldError)
	return ok
}

// ErrConcurrentUpdate indicates optimistic concurrency failure.
// This error is returned when a concurrent update to a run is detected.
var ErrConcurrentUpdate = errors.New("concurrent update")

// Resume resumes a yielded workflow run by appending a ResumeEvent and then executing the script
// from where it left off. It returns the final output of the workflow after resumption.
func (c *Client[Input, Output]) Resume(ctx context.Context, runID string, output any) (Output, error) {
	var zero Output

	// Retrieve the original input from the StartEvent to feed back into runThread
	firstEventData, err := c.store.GetFirstEvent(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("failed to fetch first event for input extraction: %w", err)
	}

	if firstEventData == nil {
		return zero, fmt.Errorf("run %s not found", runID)
	}

	var start events.Event
	if err := json.Unmarshal(firstEventData, &start); err != nil {
		return zero, fmt.Errorf("failed to unmarshal start event: %w", err)
	}

	startMeta, ok := start.Metadata.(events.StartEvent)
	if !ok {
		return zero, fmt.Errorf("first event for run %s is not StartEvent", runID)
	}

	var input Input
	inputBytes, err := json.Marshal(startMeta.Input())
	if err != nil {
		return zero, fmt.Errorf("failed to marshal stored input: %w", err)
	}
	if err := json.Unmarshal(inputBytes, &input); err != nil {
		return zero, fmt.Errorf("failed to unmarshal stored input: %w", err)
	}

	scriptHash := startMeta.ScriptHash()
	script, err := c.store.GetScript(ctx, scriptHash)
	if err != nil {
		return zero, fmt.Errorf("failed to fetch script: %w", err)
	}
	if script == nil {
		return zero, fmt.Errorf("script not found for run %s", runID)
	}

	// Fetch last event version for optimistic concurrency control
	_, version, err := c.store.GetLastEvent(ctx, runID)
	if err != nil {
		return zero, err
	}

	resumeEvent := events.NewResumeEvent(output)
	event := &events.Event{
		Timestamp: time.Now(),
		Metadata:  resumeEvent,
	}

	eventData, err := json.Marshal(event)
	if err != nil {
		return zero, fmt.Errorf("failed to marshal resume event: %w", err)
	}

	if _, err := c.store.AppendEvent(ctx, runID, version, eventData); err != nil {
		return zero, err
	}

	// Execute the workflow from where it yielded
	return runThread(ctx, c, runID, scriptHash, script, input)
}

// Context key for runID
type runIDKey struct{}

// WithRunID is an internal alias for WithRunID
func WithRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIDKey{}, runID)
}

// GetRunID extracts runID from context
func GetRunID(ctx context.Context) (string, bool) {
	runID, ok := ctx.Value(runIDKey{}).(string)
	return runID, ok
}
