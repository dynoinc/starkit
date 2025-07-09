package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/dynoinc/starkit/workflow"
	"github.com/dynoinc/starkit/workflow/events"
)

func setupTestStore(t *testing.T) *workflow.Store {
	t.Helper()
	ctx := t.Context()
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	postgresContainer, err := postgres.Run(ctx, "postgres:16-alpine", postgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { _ = postgresContainer.Terminate(t.Context()) })

	connStr := postgresContainer.MustConnectionString(ctx, "sslmode=disable")
	store, err := workflow.NewStore(ctx, connStr)
	require.NoError(t, err)
	t.Cleanup(store.Close)
	return store
}

// Test data structures for JSON-based tests
type PingRequest struct {
	Message string `json:"message"`
}

type PingResponse struct {
	Message string `json:"message"`
}

type ComplexRequest struct {
	Text   string            `json:"text"`
	Number int               `json:"number"`
	Flag   bool              `json:"flag"`
	Tags   []string          `json:"tags"`
	Meta   map[string]string `json:"meta"`
}

type ComplexResponse struct {
	Result string         `json:"result"`
	Data   map[string]any `json:"data"`
}

// WorkflowTestSuite provides a clean testing environment for starkit workflows.
type WorkflowTestSuite struct {
	suite.Suite
	store  *workflow.Store
	client *workflow.Client[PingRequest, PingResponse]
}

// SetupTest initializes a fresh client for each test.
func (s *WorkflowTestSuite) SetupTest() {
	s.store = setupTestStore(s.T())
	s.client = workflow.NewClient[PingRequest, PingResponse](s.store)

	// Register a standard ping function for tests
	workflow.RegisterFunc(s.client, s.pingPong, workflow.WithName("test.ping_pong"))
	workflow.RegisterFunc(s.client, s.yield, workflow.WithName("test.yield_execution"))
}

// Helper: pingPong is a standard test function
func (s *WorkflowTestSuite) pingPong(ctx context.Context, req PingRequest) (PingResponse, error) {
	return PingResponse{Message: "pong: " + req.Message}, nil
}

func (s *WorkflowTestSuite) yield(ctx context.Context, req PingRequest) (PingResponse, error) {
	_, err := workflow.NewYieldError(ctx)
	return PingResponse{}, err
}

// Helper: runScript executes a workflow script and returns the result
func (s *WorkflowTestSuite) runScript(script string, input PingRequest) (PingResponse, error) {
	runID := fmt.Sprintf("test-run-%d", time.Now().UnixNano())
	return s.client.Run(s.T().Context(), runID, []byte(script), input)
}

// Helper: mustRunScript executes a script and requires it to succeed
func (s *WorkflowTestSuite) mustRunScript(script string, input PingRequest) PingResponse {
	output, err := s.runScript(script, input)
	s.Require().NoError(err)
	return output
}

// Helper: getEvents retrieves events for a run (returns empty slice if not found)
func (s *WorkflowTestSuite) getEvents(runID string) []*events.Event {
	eventList, err := s.client.GetEvents(s.T().Context(), runID)
	if err != nil {
		return []*events.Event{} // Return empty slice for non-existent runs
	}
	return eventList
}

// Helper: expectEvents verifies event sequence
func (s *WorkflowTestSuite) expectEvents(runID string, expectedTypes ...events.EventType) {
	actualEvents := s.getEvents(runID)
	s.Require().Len(actualEvents, len(expectedTypes), "Event count mismatch")

	for i, expectedType := range expectedTypes {
		s.Require().Equal(expectedType, actualEvents[i].Type(), "Event %d type mismatch", i)
	}
}

// Helper: registerFunction adds a function to the client
func (s *WorkflowTestSuite) registerFunction(name string, fn any) {
	switch f := fn.(type) {
	case func(context.Context, PingRequest) (PingResponse, error):
		workflow.RegisterFunc(s.client, f, workflow.WithName(name))
	default:
		s.T().Fatalf("Unsupported function type: %T", fn)
	}
}

// Basic workflow execution with JSON
func (s *WorkflowTestSuite) TestBasicWorkflowExecution() {
	script := `
def main(ctx, input):
    return test.ping_pong(ctx=ctx, req={"message": input["message"]})
`
	output := s.mustRunScript(script, PingRequest{Message: "hello"})
	s.Equal("pong: hello", output.Message)
}

// Test function registration with custom names
func (s *WorkflowTestSuite) TestFunctionRegistration() {
	customFn := func(ctx context.Context, req PingRequest) (PingResponse, error) {
		return PingResponse{Message: "custom: " + req.Message}, nil
	}
	s.registerFunction("custom.Function", customFn)

	script := `
def main(ctx, input):
    return custom.Function(ctx=ctx, req={"message": input["message"]})
`
	output := s.mustRunScript(script, PingRequest{Message: "test"})
	s.Equal("custom: test", output.Message)
}

// Test JSON integration with various data types
func (s *WorkflowTestSuite) TestJSONDataTypes() {
	script := `
def main(ctx, input):
    # Test various JSON data types
    data = {
        "string": "hello",
        "number": 42,
        "float": 3.14,
        "bool": True,
        "null": None,
        "array": [1, 2, 3],
        "object": {"nested": "value"}
    }
    
    # Use original input message
    data["original"] = input["message"]
    
    return {"message": "data processed", "data": data}
`
	output := s.mustRunScript(script, PingRequest{Message: "test"})
	s.Equal("data processed", output.Message)
}

// Test with nil/None values
func (s *WorkflowTestSuite) TestNilAndNoneHandling() {
	nilTestFn := func(ctx context.Context, req PingRequest) (PingResponse, error) {
		if req.Message == "" {
			return PingResponse{}, nil // Return zero value for empty message (nil input)
		}
		return PingResponse{Message: "not nil: " + req.Message}, nil
	}
	s.registerFunction("test.NilTest", nilTestFn)

	script := `
def main(ctx, input):
    if input["message"] == "test_nil":
        # Test calling with nil/None
        result = test.NilTest(ctx=ctx, req=None)
        return {"message": "nil result: " + (result["message"] if result["message"] else "empty")}
    else:
        result = test.NilTest(ctx=ctx, req={"message": input["message"]})
        return {"message": result["message"]}
`

	// Test with None
	output := s.mustRunScript(script, PingRequest{Message: "test_nil"})
	s.Equal("nil result: empty", output.Message)

	// Test with actual value
	output = s.mustRunScript(script, PingRequest{Message: "hello"})
	s.Equal("not nil: hello", output.Message)
}

// Test with basic primitive types
func (s *WorkflowTestSuite) TestBasicTypes() {
	// Test function that handles different types
	typeTestFn := func(ctx context.Context, req any) (any, error) {
		switch v := req.(type) {
		case string:
			return "string: " + v, nil
		case float64: // JSON numbers are float64
			return fmt.Sprintf("number: %.0f", v), nil
		case bool:
			return fmt.Sprintf("bool: %t", v), nil
		case map[string]any:
			if msg, ok := v["message"].(string); ok {
				return map[string]any{"result": "object: " + msg}, nil
			}
			return map[string]any{"result": "object: unknown"}, nil
		case []any:
			return fmt.Sprintf("array: %d items", len(v)), nil
		default:
			return "unknown type", nil
		}
	}

	client := workflow.NewClient[any, any](s.store)
	workflow.RegisterFunc(client, typeTestFn, workflow.WithName("test.TypeTest"))

	// Test string
	script := `
def main(ctx, input):
    return test.TypeTest(ctx=ctx, req="hello world")
`
	result, err := client.Run(s.T().Context(), "test-string", []byte(script), "input")
	s.Require().NoError(err)
	s.Equal("string: hello world", result)

	// Test number
	script = `
def main(ctx, input):
    return test.TypeTest(ctx=ctx, req=42)
`
	result, err = client.Run(s.T().Context(), "test-number", []byte(script), 0)
	s.Require().NoError(err)
	s.Equal("number: 42", result)

	// Test boolean
	script = `
def main(ctx, input):
    return test.TypeTest(ctx=ctx, req=True)
`
	result, err = client.Run(s.T().Context(), "test-bool", []byte(script), false)
	s.Require().NoError(err)
	s.Equal("bool: true", result)

	// Test array
	script = `
def main(ctx, input):
    return test.TypeTest(ctx=ctx, req=[1, 2, 3, 4, 5])
`
	result, err = client.Run(s.T().Context(), "test-array", []byte(script), []int{})
	s.Require().NoError(err)
	s.Equal("array: 5 items", result)
}

// Test complex nested structures
func (s *WorkflowTestSuite) TestComplexStructures() {
	complexFn := func(ctx context.Context, req ComplexRequest) (ComplexResponse, error) {
		return ComplexResponse{
			Result: fmt.Sprintf("processed: %s (%d)", req.Text, req.Number),
			Data: map[string]any{
				"flag":     req.Flag,
				"tagCount": len(req.Tags),
				"meta":     req.Meta,
			},
		}, nil
	}

	client := workflow.NewClient[ComplexRequest, ComplexResponse](s.store)
	workflow.RegisterFunc(client, complexFn, workflow.WithName("test.Complex"))

	script := `
def main(ctx, input):
    request = {
        "text": input["text"],
        "number": input["number"],
        "flag": input["flag"],
        "tags": input["tags"],
        "meta": input["meta"]
    }
    
    result = test.Complex(ctx=ctx, req=request)
    
    # Return modified response
    result["data"]["processed"] = True
    return result
`

	input := ComplexRequest{
		Text:   "hello world",
		Number: 42,
		Flag:   true,
		Tags:   []string{"tag1", "tag2", "tag3"},
		Meta:   map[string]string{"key1": "value1", "key2": "value2"},
	}

	output, err := client.Run(s.T().Context(), "test-complex", []byte(script), input)
	s.Require().NoError(err)

	s.Equal("processed: hello world (42)", output.Result)
	s.Equal(true, output.Data["flag"])
	s.Equal(float64(3), output.Data["tagCount"]) // JSON numbers are float64
	s.Equal(true, output.Data["processed"])

	meta, ok := output.Data["meta"].(map[string]any)
	s.Require().True(ok)
	s.Equal("value1", meta["key1"])
	s.Equal("value2", meta["key2"])
}

// Error handling - function errors
func (s *WorkflowTestSuite) TestFunctionErrorHandling() {
	failingFn := func(ctx context.Context, req PingRequest) (PingResponse, error) {
		return PingResponse{}, fmt.Errorf("intentional failure: %s", req.Message)
	}
	s.registerFunction("test.FailingFunction", failingFn)

	script := `
def main(ctx, input):
    return test.FailingFunction(ctx=ctx, req={"message": input["message"]})
`
	_, err := s.runScript(script, PingRequest{Message: "fail"})
	s.Require().Error(err)
	s.Contains(err.Error(), "intentional failure: fail")
}

// Error handling - panic recovery
func (s *WorkflowTestSuite) TestPanicRecovery() {
	panicFn := func(ctx context.Context, req PingRequest) (PingResponse, error) {
		panic("test panic")
	}
	s.registerFunction("test.PanicFunction", panicFn)

	script := `
def main(ctx, input):
    return test.PanicFunction(ctx=ctx, req={"message": input["message"]})
`
	_, err := s.runScript(script, PingRequest{Message: "panic"})
	s.Require().Error(err)
	s.Contains(err.Error(), "panic")
}

// Script validation - syntax errors
func (s *WorkflowTestSuite) TestScriptSyntaxValidation() {
	invalidScript := `
def main(ctx, input)  # Missing colon
    return {"message": "test"}
`
	_, err := s.runScript(invalidScript, PingRequest{Message: "test"})
	s.Require().Error(err)
	s.Contains(err.Error(), "syntax error")
}

// Script validation - missing main function
func (s *WorkflowTestSuite) TestScriptMainFunctionValidation() {
	scriptWithoutMain := `
def helper_function(ctx, input):
    return {"message": "test"}
`
	_, err := s.runScript(scriptWithoutMain, PingRequest{Message: "test"})
	s.Require().Error(err)
	s.Contains(err.Error(), "main function")
}

// Retry policy functionality
func (s *WorkflowTestSuite) TestRetryPolicy() {
	attempts := 0
	retryFn := func(ctx context.Context, req PingRequest) (PingResponse, error) {
		attempts++
		if attempts < 3 {
			return PingResponse{}, fmt.Errorf("transient error")
		}
		return PingResponse{Message: "success after retries"}, nil
	}

	policy := backoff.WithMaxRetries(backoff.NewConstantBackOff(1*time.Millisecond), 3)
	workflow.RegisterFunc(s.client, retryFn, workflow.WithName("test.RetryFunction"), workflow.WithRetryPolicy(policy))

	script := `
def main(ctx, input):
    return test.RetryFunction(ctx=ctx, req={"message": input["message"]})
`
	output := s.mustRunScript(script, PingRequest{Message: "retry"})
	s.Equal("success after retries", output.Message)
	s.Equal(3, attempts, "Expected exactly 3 attempts")
}

// Starlark math module integration
func (s *WorkflowTestSuite) TestStarlarkMathModule() {
	script := `
load("math", "sqrt")

def main(ctx, input):
    result = sqrt(16)
    return {"message": str(result)}
`
	output := s.mustRunScript(script, PingRequest{Message: "math"})
	s.Equal("4.0", output.Message)
}

// Deterministic time function
func (s *WorkflowTestSuite) TestDeterministicTimeFunction() {
	script := `
load("time", "now")

def main(ctx, input):
    timestamp = now(ctx=ctx)
    return {"message": "timestamp: " + timestamp}
`
	output := s.mustRunScript(script, PingRequest{Message: "time"})
	s.Contains(output.Message, "timestamp:")
}

// Deterministic random function
func (s *WorkflowTestSuite) TestDeterministicRandomFunction() {
	script := `
load("rand", "int")

def main(ctx, input):
    random_num = int(ctx=ctx, max=100)
    return {"message": "random: " + str(random_num)}
`
	output := s.mustRunScript(script, PingRequest{Message: "random"})
	s.Contains(output.Message, "random:")
}

// Sleep function with duration object
func (s *WorkflowTestSuite) TestSleepFunction() {
	script := `
load("time", "sleep", "millisecond")

def main(ctx, input):
    sleep(ctx=ctx, duration=millisecond)  # 1ms
    return {"message": "slept"}
`
	output := s.mustRunScript(script, PingRequest{Message: "sleep"})
	s.Equal("slept", output.Message)
}

// Sleep function with parsed duration
func (s *WorkflowTestSuite) TestSleepFunctionWithParsedDuration() {
	script := `
load("time", "sleep", "parse_duration")

def main(ctx, input):
    sleep(ctx=ctx, duration=parse_duration("1ms"))  # 1ms as parsed duration
    return {"message": "slept"}
`
	output := s.mustRunScript(script, PingRequest{Message: "sleep"})
	s.Equal("slept", output.Message)
}

// Sleep function with Duration object from time module
func (s *WorkflowTestSuite) TestSleepFunctionWithDuration() {
	script := `
load("time", "sleep", "parse_duration", "millisecond")

def main(ctx, input):
    # Sleep using parsed duration
    duration1 = parse_duration("1ms")
    sleep(ctx=ctx, duration=duration1)
    
    # Sleep using duration constant
    sleep(ctx=ctx, duration=millisecond)
    
    return {"message": "slept with duration objects"}
`
	output := s.mustRunScript(script, PingRequest{Message: "duration-sleep"})
	s.Equal("slept with duration objects", output.Message)
}

// Sleep function should reject non-Duration types
func (s *WorkflowTestSuite) TestSleepFunctionRejectsNonDuration() {
	script := `
load("time", "sleep")

def main(ctx, input):
    sleep(ctx=ctx, duration=1.0)  # This should fail - not a Duration object
    return {"message": "should not reach here"}
`
	_, err := s.client.Run(s.T().Context(), "test-reject-non-duration", []byte(script), PingRequest{Message: "test"})
	s.Require().Error(err)
	s.Contains(err.Error(), "duration must be a Duration object from the time module")
}

// Multiple function calls in sequence
func (s *WorkflowTestSuite) TestMultipleFunctionCalls() {
	firstFn := func(ctx context.Context, req PingRequest) (PingResponse, error) {
		return PingResponse{Message: "first: " + req.Message}, nil
	}
	secondFn := func(ctx context.Context, req PingRequest) (PingResponse, error) {
		return PingResponse{Message: "second: " + req.Message}, nil
	}

	s.registerFunction("test.FirstFunction", firstFn)
	s.registerFunction("test.SecondFunction", secondFn)

	script := `
def main(ctx, input):
    first_result = test.FirstFunction(ctx=ctx, req={"message": input["message"]})
    second_result = test.SecondFunction(ctx=ctx, req={"message": first_result["message"]})
    
    return {"message": "final: " + second_result["message"]}
`
	output := s.mustRunScript(script, PingRequest{Message: "chained"})
	s.Equal("final: second: first: chained", output.Message)
}

// Yield and resume functionality
func (s *WorkflowTestSuite) TestYieldAndResume() {
	script := `
def main(ctx, input):
    r1 = test.ping_pong(ctx=ctx, req={"message": "initial"})
    test.yield_execution(ctx=ctx, req=None)
    r2 = test.ping_pong(ctx=ctx, req={"message": "resumed"})
    return r2
`
	runID := "test-yield-resume"
	_, err := s.client.Run(s.T().Context(), runID, []byte(script), PingRequest{})
	s.Require().Error(err)
	var yieldErr *workflow.YieldError
	s.Require().True(errors.As(err, &yieldErr), "expected yield error, got %v", err)

	// Resume the workflow
	output, err := s.client.Resume(s.T().Context(), runID, nil)
	s.Require().NoError(err)
	s.Equal("pong: resumed", output.Message)
}

// Test event recording for a simple workflow
func (s *WorkflowTestSuite) TestEventRecording() {
	script := `
def main(ctx, input):
    return test.ping_pong(ctx=ctx, req={"message": input["message"]})
`
	runID := "event-test-run"
	_, err := s.client.Run(s.T().Context(), runID, []byte(script), PingRequest{Message: "events"})
	s.Require().NoError(err)

	// Verify basic event sequence
	s.expectEvents(runID,
		events.EventTypeStart,
		events.EventTypeCall,
		events.EventTypeReturn,
		events.EventTypeFinish,
	)
}

// Test JSON encoding/decoding in scripts
func (s *WorkflowTestSuite) TestJSONEncodingDecoding() {
	script := `
load("json", "encode", "decode")

def main(ctx, input):
    # Create complex data structure
    data = {
        "input": input,
        "processing": {
            "step1": "encode to JSON",
            "step2": "decode from JSON"
        },
        "numbers": [1, 2, 3.14, 42],
        "flags": [True, False, None]
    }
    
    # Encode to JSON string
    json_str = encode(data)
    
    # Decode back from JSON  
    decoded = decode(json_str)
    
    # Verify round-trip works
    return {
        "message": "JSON test: " + decoded["input"]["message"]
    }
`
	output := s.mustRunScript(script, PingRequest{Message: "json-test"})
	s.Equal("JSON test: json-test", output.Message)
}

// Comprehensive test with multiple yield points, time functions, retries, and event verification
func (s *WorkflowTestSuite) TestComplexWorkflowWithMultipleYieldsAndRetries() {
	var attempt int
	flakyFn := func(ctx context.Context, req PingRequest) (PingResponse, error) {
		attempt++
		if attempt < 3 {
			return PingResponse{}, fmt.Errorf("transient error")
		}
		return PingResponse{Message: "succeeded on attempt 3"}, nil
	}

	client := workflow.NewClient[PingRequest, PingResponse](s.store)
	workflow.RegisterFunc(client, flakyFn,
		workflow.WithName("test.flaky"),
		workflow.WithRetryPolicy(backoff.NewConstantBackOff(1*time.Millisecond)),
	)
	workflow.RegisterFunc(client, s.pingPong, workflow.WithName("test.ping_pong"))
	workflow.RegisterFunc(client, s.yield, workflow.WithName("test.yield_execution"))

	script := `
def main(ctx, input):
    r1 = test.ping_pong(ctx=ctx, req={"message": "step 1"})
    test.yield_execution(ctx=ctx, req=None)
    r2 = test.flaky(ctx=ctx, req={"message": "step 2"})
    test.yield_execution(ctx=ctx, req=None)
    r3 = test.ping_pong(ctx=ctx, req={"message": r2["message"] + " and done"})
    return r3
`
	runID := "test-complex-workflow"

	// Start the workflow
	_, err := client.Run(s.T().Context(), runID, []byte(script), PingRequest{})
	s.Require().Error(err)
	var yieldErr *workflow.YieldError
	s.Require().True(errors.As(err, &yieldErr))

	// Resume the workflow
	_, err = client.Resume(s.T().Context(), runID, nil)
	s.Require().Error(err)
	s.Require().True(errors.As(err, &yieldErr))

	// Final resume
	output, err := client.Resume(s.T().Context(), runID, nil)
	s.Require().NoError(err)
	s.Equal("pong: succeeded on attempt 3 and done", output.Message)
	s.Equal(3, attempt, "Expected flaky function to be called 3 times")

	// Verify final events
	eventList, err := client.GetEvents(s.T().Context(), runID)
	s.Require().NoError(err)
	s.Equal(events.EventTypeFinish, eventList[len(eventList)-1].Type())
}

// Test Starlark time module with official functions
func (s *WorkflowTestSuite) TestStarlarkTimeModule() {
	script := `
load("time", "parse_duration", "from_timestamp", "second", "minute", "hour")

def main(ctx, input):
    # Test duration constants
    one_second = second
    one_minute = minute
    one_hour = hour
    
    # Test parse_duration
    duration = parse_duration("5s")
    
    # Test from_timestamp
    timestamp = from_timestamp(1640995200)  # 2022-01-01 00:00:00 UTC
    
    return {
        "message": "Time module: duration=" + str(type(duration)) + ", timestamp=" + str(type(timestamp)) + ", constants=" + str(one_second) + " " + str(one_minute) + " " + str(one_hour)
    }
`
	output := s.mustRunScript(script, PingRequest{Message: "time-module"})
	s.Contains(output.Message, "Time module:")
	s.Contains(output.Message, "duration")
	s.Contains(output.Message, "time")
	s.Contains(output.Message, "1s")
}

// Test that context from client is passed to registered functions
func (s *WorkflowTestSuite) TestContextPassedToRegisteredFunctions() {
	// Register a function that extracts information from the context
	contextTestFn := func(ctx context.Context, req PingRequest) (PingResponse, error) {
		// Extract run ID from context to verify it's the correct context
		runID, ok := workflow.GetRunID(ctx)
		if !ok {
			return PingResponse{}, fmt.Errorf("no run ID found in context")
		}

		// Check for the test value in context
		testValue := ctx.Value(WorkflowTestSuite{})
		if testValue == nil {
			return PingResponse{}, fmt.Errorf("no WorkflowTestSuite found in context")
		}

		return PingResponse{Message: fmt.Sprintf("context_test_passed_runid_%s", runID)}, nil
	}

	s.registerFunction("test.ContextTest", contextTestFn)

	script := `
def main(ctx, input):
    result = test.ContextTest(ctx=ctx, req={"message": input["message"]})
    return {"message": result["message"]}
`

	runID := "context-test-run-123"
	ctx := context.WithValue(s.T().Context(), WorkflowTestSuite{}, "fake-value")
	output, err := s.client.Run(ctx, runID, []byte(script), PingRequest{Message: "test"})
	s.Require().NoError(err)
	s.Contains(output.Message, "context_test_passed_runid_"+runID)
}

func TestWorkflowTestSuite(t *testing.T) {
	suite.Run(t, new(WorkflowTestSuite))
}

func TestValidateInvariants(t *testing.T) {
	runID := "test-run"

	// Test cases for event transitions
	testCases := []struct {
		name      string
		lastEvent events.EventMetadata
		newEvent  events.EventMetadata
		expectErr bool
	}{
		// Valid transitions
		{"Start a new run", nil, events.NewStartEvent("hash", nil), false},
		{"Call from Start", events.NewStartEvent("hash", nil), events.NewCallEvent("f", nil), false},
		{"Return from Call", events.NewCallEvent("f", nil), events.NewReturnEvent("out", nil), false},
		{"Yield from Call", events.NewCallEvent("f", nil), events.NewYieldEvent(runID), false},
		{"Resume from Yield", events.NewYieldEvent(runID), events.NewResumeEvent(nil), false},
		{"Call after Return", events.NewReturnEvent("out", nil), events.NewCallEvent("f", nil), false},
		{"Finish from Return", events.NewReturnEvent("out", nil), events.NewFinishEvent("final", nil), false},
		{"Call after Resume", events.NewResumeEvent(nil), events.NewCallEvent("f", nil), false},

		// Invalid transitions
		{"Start an existing run", events.NewStartEvent("hash", nil), events.NewStartEvent("hash", nil), true},
		{"Start on a finished run", events.NewFinishEvent("out", nil), events.NewStartEvent("hash", nil), true},
		{"Event on a finished run", events.NewFinishEvent("out", nil), events.NewCallEvent("f", nil), true},
		{"Event without a start", nil, events.NewCallEvent("f", nil), true},
		{"Invalid Yield (from Start)", events.NewStartEvent("hash", nil), events.NewYieldEvent(runID), true},
		{"Invalid Resume (not yielded)", events.NewReturnEvent("out", nil), events.NewResumeEvent(nil), true},
		{"Invalid Return (not called)", events.NewStartEvent("hash", nil), events.NewReturnEvent("out", nil), true},
		{"Invalid transition from Yield", events.NewYieldEvent(runID), events.NewReturnEvent(nil, nil), true},
		{"Invalid transition from Call", events.NewCallEvent("f", nil), events.NewFinishEvent(nil, nil), true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := workflow.ValidateInvariants(runID, tc.lastEvent, tc.newEvent)
			if tc.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestYieldError(t *testing.T) {
	err := &workflow.YieldError{}
	var target *workflow.YieldError
	require.True(t, errors.As(err, &target))
	require.Equal(t, err, target)
}
