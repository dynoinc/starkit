package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	starjson "go.starlark.net/lib/json"
	"go.starlark.net/lib/math"
	startime "go.starlark.net/lib/time"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/dynoinc/starflow/workflow/events"
)

type trace struct {
	runID string
	store *Store

	events      []*events.Event
	nextEventID int
	lastEvent   events.EventMetadata // Track last event for invariant validation
}

func newTrace(runID string, store *Store, eventList []*events.Event) *trace {
	lastEvent := events.EventMetadata(nil)
	if len(eventList) > 0 {
		lastEvent = eventList[len(eventList)-1].Metadata
	}

	return &trace{
		runID:       runID,
		store:       store,
		events:      eventList,
		nextEventID: len(eventList),
		lastEvent:   lastEvent,
	}
}

func popEvent[ET events.EventMetadata](t *trace) (ET, bool) {
	var zero ET
	if len(t.events) == 0 {
		return zero, false
	}

	nextEvent := t.events[0]
	if nextEvent.Type() != zero.EventType() {
		return zero, false
	}

	t.events = t.events[1:]
	return nextEvent.Metadata.(ET), true
}

// ValidateInvariants validates business logic rules for event transitions.
// This contains all the invariants that were previously in the Store implementations.
func ValidateInvariants(runID string, lastEvent events.EventMetadata, newEvent events.EventMetadata) error {
	if runID == "" {
		return fmt.Errorf("runID must not be empty")
	}

	if newEvent.EventType() == events.EventTypeStart {
		if lastEvent != nil {
			return fmt.Errorf("run %s already exists", runID)
		}
		return nil
	}

	if lastEvent == nil {
		return fmt.Errorf("run %s not found", runID)
	}

	if lastEvent.EventType() == events.EventTypeFinish {
		return fmt.Errorf("run %s has already finished", runID)
	}

	switch newEvent.(type) {
	case events.ResumeEvent:
		if lastEvent.EventType() != events.EventTypeYield {
			return fmt.Errorf("run %s is not in yielded state", runID)
		}
	case events.YieldEvent:
		if lastEvent.EventType() != events.EventTypeCall {
			return fmt.Errorf("invalid event type: %s -> %s not allowed", lastEvent.EventType(), newEvent.EventType())
		}
	case events.ReturnEvent:
		if lastEvent.EventType() != events.EventTypeCall {
			return fmt.Errorf("invalid event type: %s -> %s not allowed", lastEvent.EventType(), newEvent.EventType())
		}
	default:
		if lastEvent.EventType() == events.EventTypeCall {
			return fmt.Errorf("invalid event type: %s -> %s not allowed", lastEvent.EventType(), newEvent.EventType())
		}
		if lastEvent.EventType() == events.EventTypeYield {
			return fmt.Errorf("invalid event type: %s -> %s not allowed", lastEvent.EventType(), newEvent.EventType())
		}
	}

	return nil
}

func recordEvent[ET events.EventMetadata](ctx context.Context, t *trace, event ET) error {
	// If we have existing events (replay mode), validate against them
	if expected, ok := popEvent[ET](t); ok {
		if !reflect.DeepEqual(expected, event) {
			return fmt.Errorf("event mismatch: expected %+v, got %+v", expected, event)
		}
		return nil
	}

	// If we're in recording mode, ensure we've consumed all events
	if len(t.events) != 0 {
		return fmt.Errorf("trying to record event %s, but there are %d events left in the trace", event.EventType(), len(t.events))
	}

	// Check invariants first
	if err := ValidateInvariants(t.runID, t.lastEvent, event); err != nil {
		return err
	}

	newEvent := &events.Event{
		Timestamp: time.Now(),
		Metadata:  event,
	}

	// Serialize to bytes - normalization happens automatically in JSON marshaling
	eventData, err := json.Marshal(newEvent)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	// Delegate to store with OCC
	nextEventID, err := t.store.AppendEvent(ctx, t.runID, t.nextEventID, eventData)
	if err != nil {
		return fmt.Errorf("failed to record event: %w", err)
	}

	t.nextEventID = nextEventID
	t.lastEvent = event
	return nil
}

// starlarkModule represents a module in Starlark
type starlarkModule struct {
	name    string
	members starlark.StringDict
}

func (m *starlarkModule) String() string        { return fmt.Sprintf("<module %s>", m.name) }
func (m *starlarkModule) Type() string          { return "module" }
func (m *starlarkModule) Freeze()               { m.members.Freeze() }
func (m *starlarkModule) Truth() starlark.Bool  { return starlark.True }
func (m *starlarkModule) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable type: module") }

// Attr returns the value of the specified attribute
func (m *starlarkModule) Attr(name string) (starlark.Value, error) {
	if value, ok := m.members[name]; ok {
		return value, nil
	}
	return nil, nil // Return nil for missing attributes
}

// AttrNames returns the names of all attributes
func (m *starlarkModule) AttrNames() []string {
	names := make([]string, 0, len(m.members))
	for name := range m.members {
		names = append(names, name)
	}
	return names
}

func globals(t *trace, registry map[string]registeredFn) (starlark.StringDict, error) {
	globals := make(starlark.StringDict)

	// Group functions by module
	modules := make(map[string]starlark.StringDict)

	for name, regFn := range registry {
		parts := strings.Split(name, ".")
		moduleName := parts[0]
		funcName := parts[1]

		if modules[moduleName] == nil {
			modules[moduleName] = make(starlark.StringDict)
		}
		modules[moduleName][funcName] = wrapFn(t, regFn)
	}

	// Add modules to globals
	for moduleName, moduleDict := range modules {
		moduleDict.Freeze()
		globals[moduleName] = &starlarkModule{name: moduleName, members: moduleDict}
	}

	return globals, nil
}

// makeSleepBuiltin returns a starlark builtin implementing durable sleep.
func makeSleepBuiltin(t *trace) *starlark.Builtin {
	return starlark.NewBuiltin("sleep", func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var ctxVal starlark.Value
		var durationVal starlark.Value
		if err := starlark.UnpackArgs("sleep", args, kwargs, "ctx", &ctxVal, "duration", &durationVal); err != nil {
			return nil, err
		}

		starlarkCtx, ok := ctxVal.(*starlarkContext)
		if !ok {
			return nil, fmt.Errorf("first argument must be context")
		}

		duration, ok := durationVal.(startime.Duration)
		if !ok {
			return nil, fmt.Errorf("duration must be a Duration object from the time module")
		}

		sleepDuration := time.Duration(duration)
		if sleepEvent, ok := popEvent[events.SleepEvent](t); ok {
			sleepDuration = time.Until(sleepEvent.WakeupAt())
		} else {
			if err := recordEvent(starlarkCtx.ctx, t, events.NewSleepEvent(time.Now().Add(sleepDuration))); err != nil {
				return nil, err
			}
		}

		select {
		case <-starlarkCtx.ctx.Done():
			return nil, starlarkCtx.ctx.Err()
		case <-time.After(sleepDuration):
		}

		return starlark.None, nil
	})
}

// makeTimeNowBuiltin returns a starlark builtin implementing deterministic time.now.
func makeTimeNowBuiltin(t *trace) *starlark.Builtin {
	return starlark.NewBuiltin("now", func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var ctxVal starlark.Value
		if err := starlark.UnpackArgs("now", args, kwargs, "ctx", &ctxVal); err != nil {
			return nil, err
		}

		starlarkCtx, ok := ctxVal.(*starlarkContext)
		if !ok {
			return nil, fmt.Errorf("first argument must be context")
		}

		var timestamp time.Time
		if timeNowEvent, ok := popEvent[events.TimeNowEvent](t); ok {
			timestamp = timeNowEvent.Timestamp()
		} else {
			timestamp = time.Now()

			if err := recordEvent(starlarkCtx.ctx, t, events.NewTimeNowEvent(timestamp)); err != nil {
				return nil, err
			}
		}

		// Return timestamp as ISO string
		return starlark.String(timestamp.Format(time.RFC3339Nano)), nil
	})
}

// makeRandIntBuiltin returns a starlark builtin implementing deterministic rand.int.
func makeRandIntBuiltin(t *trace) *starlark.Builtin {
	return starlark.NewBuiltin("int", func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var ctxVal starlark.Value
		var maxVal starlark.Value
		if err := starlark.UnpackArgs("int", args, kwargs, "ctx", &ctxVal, "max", &maxVal); err != nil {
			return nil, err
		}

		starlarkCtx, ok := ctxVal.(*starlarkContext)
		if !ok {
			return nil, fmt.Errorf("first argument must be context")
		}

		max, ok := maxVal.(starlark.Int)
		if !ok {
			return nil, fmt.Errorf("max must be an integer")
		}

		maxInt64, ok := max.Int64()
		if !ok {
			return nil, fmt.Errorf("max value too large")
		}

		var result int64
		if randIntEvent, ok := popEvent[events.RandIntEvent](t); ok {
			result = randIntEvent.Result()
		} else {
			result = rand.Int63n(maxInt64)

			if err := recordEvent(starlarkCtx.ctx, t, events.NewRandIntEvent(result)); err != nil {
				return nil, err
			}
		}

		return starlark.MakeInt64(result), nil
	})
}

func runThread[Input any, Output any](
	ctx context.Context,
	c *Client[Input, Output],
	runID string,
	scriptHash string,
	script []byte,
	input Input,
) (Output, error) {
	var zero Output
	eventDataList, err := c.store.GetEvents(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("failed to get events: %w", err)
	}

	// Convert byte slices back to Event structs
	// Events from storage are already normalized because they went through JSON marshaling
	eventList := make([]*events.Event, len(eventDataList))
	for i, eventData := range eventDataList {
		var event events.Event
		if err := json.Unmarshal(eventData, &event); err != nil {
			return zero, fmt.Errorf("failed to unmarshal event %d: %w", i, err)
		}
		eventList[i] = &event
	}

	t := newTrace(runID, c.store, eventList)

	thread := &starlark.Thread{
		Name:  fmt.Sprintf("run-%s", runID),
		Print: func(_ *starlark.Thread, msg string) { fmt.Println(msg) },
		Load: func(thread *starlark.Thread, module string) (starlark.StringDict, error) {
			if module == "time" {
				members := make(starlark.StringDict)
				maps.Copy(members, startime.Module.Members)
				members["now"] = makeTimeNowBuiltin(t)
				members["sleep"] = makeSleepBuiltin(t)
				members.Freeze()
				return members, nil
			}
			if module == "rand" {
				members := make(starlark.StringDict)
				members["int"] = makeRandIntBuiltin(t)
				members.Freeze()
				return members, nil
			}
			if module == "math" {
				return math.Module.Members, nil
			}
			if module == "json" {
				return starjson.Module.Members, nil
			}

			return nil, fmt.Errorf("module %q not found", module)
		},
	}

	globals, err := globals(t, c.registry)
	if err != nil {
		return zero, fmt.Errorf("failed to get globals: %w", err)
	}

	globalsAfterExec, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, "script", script, globals)
	if err != nil {
		return zero, fmt.Errorf("starlark execution failed: %w", err)
	}

	mainVal, ok := globalsAfterExec["main"]
	if !ok {
		return zero, fmt.Errorf("starlark script must have a main function")
	}

	mainFn, ok := mainVal.(starlark.Callable)
	if !ok {
		return zero, fmt.Errorf("main must be a function")
	}

	// Create starlark context and convert input to Starlark value
	ctxWithRunID := WithRunID(ctx, runID)
	starlarkCtx := &starlarkContext{ctx: ctxWithRunID}

	starlarkInput, err := jsonToStarlark(input)
	if err != nil {
		return zero, fmt.Errorf("failed to convert input to starlark: %w", err)
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return zero, fmt.Errorf("failed to marshal input: %w", err)
	}

	var normalizedInput any
	if err := json.Unmarshal(inputJSON, &normalizedInput); err != nil {
		return zero, fmt.Errorf("failed to unmarshal input: %w", err)
	}

	// Record start event
	if err := recordEvent(ctxWithRunID, t, events.NewStartEvent(scriptHash, normalizedInput)); err != nil {
		return zero, fmt.Errorf("failed to record start event: %w", err)
	}

	// Call main with context and input
	starlarkOutput, err := starlark.Call(thread, mainFn, starlark.Tuple{starlarkCtx, starlarkInput}, nil)
	if err != nil {
		var yerr *YieldError
		if errors.As(err, &yerr) {
			return zero, err
		}

		if recordErr := recordEvent(ctxWithRunID, t, events.NewFinishEvent(nil, err)); recordErr != nil {
			return zero, fmt.Errorf("failed to record finish event with error: %w", recordErr)
		}

		return zero, fmt.Errorf("error calling main function: %w", err)
	}

	// Convert output back to Go type
	var output Output
	if starlarkOutput != starlark.None {
		outputData, err := starlarkToJSON(starlarkOutput)
		if err != nil {
			return zero, fmt.Errorf("failed to convert output from starlark: %w", err)
		}

		// Convert via JSON marshaling
		outputBytes, err := json.Marshal(outputData)
		if err != nil {
			return zero, fmt.Errorf("failed to marshal output: %w", err)
		}

		if err := json.Unmarshal(outputBytes, &output); err != nil {
			return zero, fmt.Errorf("failed to unmarshal output: %w", err)
		}
	}

	// Record finish event
	outputData, _ := starlarkToJSON(starlarkOutput)
	if err := recordEvent(ctxWithRunID, t, events.NewFinishEvent(outputData, nil)); err != nil {
		return zero, fmt.Errorf("failed to record finish event: %w", err)
	}

	return output, nil
}

// Helper functions for Starlark <-> JSON conversion
func jsonToStarlark(data any) (starlark.Value, error) {
	switch v := data.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(v), nil
	case int:
		return starlark.MakeInt(v), nil
	case int64:
		return starlark.MakeInt64(v), nil
	case float64:
		return starlark.Float(v), nil
	case string:
		return starlark.String(v), nil
	case []any:
		list := make([]starlark.Value, len(v))
		for i, item := range v {
			val, err := jsonToStarlark(item)
			if err != nil {
				return nil, err
			}
			list[i] = val
		}
		return starlark.NewList(list), nil
	case map[string]any:
		dict := starlark.NewDict(len(v))
		for key, value := range v {
			val, err := jsonToStarlark(value)
			if err != nil {
				return nil, err
			}
			dict.SetKey(starlark.String(key), val)
		}
		return dict, nil
	default:
		// Try to convert via JSON for other types
		jsonBytes, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("unsupported type %T: %w", data, err)
		}

		var jsonData any
		if err := json.Unmarshal(jsonBytes, &jsonData); err != nil {
			return nil, err
		}

		return jsonToStarlark(jsonData)
	}
}

func starlarkToJSON(value starlark.Value) (any, error) {
	switch v := value.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(v), nil
	case starlark.Int:
		i, ok := v.Int64()
		if ok {
			return i, nil
		}
		return v.String(), nil // fallback for big integers
	case starlark.Float:
		return float64(v), nil
	case starlark.String:
		return string(v), nil
	case *starlark.List:
		result := make([]any, v.Len())
		for i := 0; i < v.Len(); i++ {
			val, err := starlarkToJSON(v.Index(i))
			if err != nil {
				return nil, err
			}
			result[i] = val
		}
		return result, nil
	case *starlark.Dict:
		result := make(map[string]any)
		for _, k := range v.Keys() {
			key, ok := k.(starlark.String)
			if !ok {
				return nil, fmt.Errorf("dict key must be string, got %s", k.Type())
			}
			val, _, err := v.Get(k)
			if err != nil {
				return nil, err
			}
			jsonVal, err := starlarkToJSON(val)
			if err != nil {
				return nil, err
			}
			result[string(key)] = jsonVal
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported starlark type: %s", v.Type())
	}
}

// starlarkContext wraps Go context for use in Starlark
type starlarkContext struct {
	ctx context.Context
}

func (sc *starlarkContext) String() string        { return "context" }
func (sc *starlarkContext) Type() string          { return "context" }
func (sc *starlarkContext) Freeze()               {}
func (sc *starlarkContext) Truth() starlark.Bool  { return starlark.True }
func (sc *starlarkContext) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable type: context") }

// wrapFn wraps a Go function to be callable from Starlark with JSON
func wrapFn(t *trace, regFn registeredFn) *starlark.Builtin {
	return starlark.NewBuiltin(regFn.name, func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		var ctxVal starlark.Value
		var reqVal starlark.Value
		if err := starlark.UnpackArgs(regFn.name, args, kwargs, "ctx", &ctxVal, "req", &reqVal); err != nil {
			return starlark.None, err
		}

		starlarkCtx, ok := ctxVal.(*starlarkContext)
		if !ok {
			return starlark.None, fmt.Errorf("first argument must be context, got %s", ctxVal.Type())
		}

		// Convert Starlark value to any
		var req any
		if reqVal != starlark.None {
			var err error
			req, err = starlarkToJSON(reqVal)
			if err != nil {
				return starlark.None, fmt.Errorf("failed to convert request: %w", err)
			}
		}

		// Record call event
		if err := recordEvent(starlarkCtx.ctx, t, events.NewCallEvent(regFn.name, req)); err != nil {
			return starlark.None, fmt.Errorf("failed to record call event: %w", err)
		}

		// Check for replay
		if returnEvent, ok := popEvent[events.ReturnEvent](t); ok {
			if _, retErr := returnEvent.Output(); retErr != nil {
				return starlark.None, retErr
			}

			if output, _ := returnEvent.Output(); output != nil {
				starlarkRes, err := jsonToStarlark(output)
				if err != nil {
					return starlark.None, fmt.Errorf("failed to convert return event output: %w", err)
				}
				return starlarkRes, nil
			}
		}

		// Check for yield/resume
		if _, ok := popEvent[events.YieldEvent](t); ok {
			resumeEvent, ok := popEvent[events.ResumeEvent](t)
			if !ok {
				// still waiting for resume
				runID, _ := GetRunID(starlarkCtx.ctx)
				return starlark.None, &YieldError{runID: runID}
			}

			if resumeEvent.Output() != nil {
				starlarkRes, err := jsonToStarlark(resumeEvent.Output())
				if err != nil {
					return starlark.None, fmt.Errorf("failed to convert resume event output: %w", err)
				}
				return starlarkRes, nil
			}

			return starlark.None, nil
		}

		// Execute function
		var resp any
		callFunc := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()

			resp, err = regFn.fn(starlarkCtx.ctx, req)
			if err != nil {
				if errors.Is(err, &YieldError{}) {
					return backoff.Permanent(err)
				}

				return err
			}

			return nil
		}

		var callErr error
		if regFn.retryPolicy != nil {
			policy := backoff.WithContext(regFn.retryPolicy, starlarkCtx.ctx)
			callErr = backoff.Retry(callFunc, policy)
		} else {
			callErr = callFunc()
		}

		// Record return event
		var event events.EventMetadata
		var yerr *YieldError
		if errors.As(callErr, &yerr) {
			runID, _ := GetRunID(starlarkCtx.ctx)
			event = events.NewYieldEvent(runID)
		} else if callErr != nil {
			event = events.NewReturnEvent(nil, callErr)
		} else {
			event = events.NewReturnEvent(resp, nil)
		}

		if err := recordEvent(starlarkCtx.ctx, t, event); err != nil {
			return starlark.None, err
		}

		if callErr != nil {
			return starlark.None, callErr
		}

		// Convert response to Starlark
		starlarkRes, err := jsonToStarlark(resp)
		if err != nil {
			return starlark.None, fmt.Errorf("failed to convert response to starlark: %w", err)
		}

		return starlarkRes, nil
	})
}
