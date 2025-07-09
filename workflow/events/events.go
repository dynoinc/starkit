package events

import (
	"encoding/json"
	"fmt"
	"time"
)

// EventType represents the type of an event in the execution history.
type EventType string

const (
	EventTypeStart  EventType = "START"
	EventTypeFinish EventType = "FINISH"

	EventTypeCall   EventType = "CALL"
	EventTypeReturn EventType = "RETURN"

	EventTypeYield  EventType = "YIELD"
	EventTypeResume EventType = "RESUME"

	EventTypeSleep   EventType = "SLEEP"
	EventTypeTimeNow EventType = "TIME_NOW"
	EventTypeRandInt EventType = "RAND_INT"
)

type EventMetadata interface {
	EventType() EventType
}

// CallEvent metadata
type CallEvent struct {
	functionName string
	input        any
}

func NewCallEvent(functionName string, input any) CallEvent {
	return CallEvent{functionName: functionName, input: input}
}

func (c CallEvent) EventType() EventType { return EventTypeCall }
func (c CallEvent) FunctionName() string { return c.functionName }
func (c CallEvent) Input() any           { return c.input }

func (c CallEvent) MarshalJSON() ([]byte, error) {
	normalizedInput := c.input
	if c.input != nil {
		normalized, err := normalizeData(c.input)
		if err != nil {
			return nil, fmt.Errorf("failed to normalize call event input: %w", err)
		}
		normalizedInput = normalized
	}

	return json.Marshal(map[string]any{
		"functionName": c.functionName,
		"input":        normalizedInput,
	})
}

func (c *CallEvent) UnmarshalJSON(data []byte) error {
	var aux struct {
		FunctionName string `json:"functionName"`
		Input        any    `json:"input"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	c.functionName = aux.FunctionName
	c.input = aux.Input
	return nil
}

// ReturnEvent metadata
type ReturnEvent struct {
	output any
	err    error
}

func NewReturnEvent(output any, err error) ReturnEvent {
	return ReturnEvent{output: output, err: err}
}

func (r ReturnEvent) EventType() EventType { return EventTypeReturn }
func (r ReturnEvent) Output() (any, error) { return r.output, r.err }

func (r ReturnEvent) MarshalJSON() ([]byte, error) {
	var errStr string
	if r.err != nil {
		errStr = r.err.Error()
	}

	normalizedOutput := r.output
	if r.output != nil {
		normalized, err := normalizeData(r.output)
		if err != nil {
			return nil, fmt.Errorf("failed to normalize return event output: %w", err)
		}
		normalizedOutput = normalized
	}

	return json.Marshal(map[string]any{
		"output": normalizedOutput,
		"error":  errStr,
	})
}

func (r *ReturnEvent) UnmarshalJSON(data []byte) error {
	var aux struct {
		Output any    `json:"output"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	r.output = aux.Output
	if aux.Error != "" {
		r.err = fmt.Errorf("%s", aux.Error)
	}
	return nil
}

// SleepEvent
type SleepEvent struct {
	wakeupAt time.Time
}

func NewSleepEvent(wakeupAt time.Time) SleepEvent {
	return SleepEvent{wakeupAt: wakeupAt}
}

func (s SleepEvent) EventType() EventType { return EventTypeSleep }
func (s SleepEvent) WakeupAt() time.Time  { return s.wakeupAt }

func (s SleepEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"wakeupAt": s.wakeupAt,
	})
}

func (s *SleepEvent) UnmarshalJSON(data []byte) error {
	var aux struct {
		WakeupAt time.Time `json:"wakeupAt"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	s.wakeupAt = aux.WakeupAt
	return nil
}

// TimeNowEvent
type TimeNowEvent struct {
	timestamp time.Time
}

func NewTimeNowEvent(timestamp time.Time) TimeNowEvent {
	return TimeNowEvent{timestamp: timestamp}
}

func (t TimeNowEvent) EventType() EventType { return EventTypeTimeNow }
func (t TimeNowEvent) Timestamp() time.Time { return t.timestamp }

func (t TimeNowEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"timestamp": t.timestamp,
	})
}

func (t *TimeNowEvent) UnmarshalJSON(data []byte) error {
	var aux struct {
		Timestamp time.Time `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	t.timestamp = aux.Timestamp
	return nil
}

// RandIntEvent
type RandIntEvent struct {
	result int64
}

func NewRandIntEvent(result int64) RandIntEvent {
	return RandIntEvent{result: result}
}

func (r RandIntEvent) EventType() EventType { return EventTypeRandInt }
func (r RandIntEvent) Result() int64        { return r.result }

func (r RandIntEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"result": r.result,
	})
}

func (r *RandIntEvent) UnmarshalJSON(data []byte) error {
	var aux struct {
		Result int64 `json:"result"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	r.result = aux.Result
	return nil
}

// YieldEvent
type YieldEvent struct {
	runID string
}

func NewYieldEvent(runID string) YieldEvent {
	return YieldEvent{runID: runID}
}

func (y YieldEvent) EventType() EventType { return EventTypeYield }
func (y YieldEvent) RunID() string        { return y.runID }

func (y YieldEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"runID": y.runID,
	})
}

func (y *YieldEvent) UnmarshalJSON(data []byte) error {
	var aux struct {
		RunID string `json:"runID"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	y.runID = aux.RunID
	return nil
}

// ResumeEvent - now uses any for maximum flexibility
type ResumeEvent struct {
	output any // Can be any JSON-serializable value
}

func NewResumeEvent(output any) ResumeEvent {
	return ResumeEvent{output: output}
}

func (r ResumeEvent) EventType() EventType { return EventTypeResume }
func (r ResumeEvent) Output() any          { return r.output }

func (r ResumeEvent) MarshalJSON() ([]byte, error) {
	normalizedOutput := r.output
	if r.output != nil {
		normalized, err := normalizeData(r.output)
		if err != nil {
			return nil, fmt.Errorf("failed to normalize resume event output: %w", err)
		}
		normalizedOutput = normalized
	}

	return json.Marshal(map[string]any{
		"output": normalizedOutput,
	})
}

func (r *ResumeEvent) UnmarshalJSON(data []byte) error {
	var aux struct {
		Output any `json:"output"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	r.output = aux.Output
	return nil
}

// FinishEvent
type FinishEvent struct {
	output any
	err    error
}

func NewFinishEvent(output any, err error) FinishEvent {
	return FinishEvent{output: output, err: err}
}

func (f FinishEvent) EventType() EventType { return EventTypeFinish }
func (f FinishEvent) Output() (any, error) { return f.output, f.err }

func (f FinishEvent) MarshalJSON() ([]byte, error) {
	var errStr string
	if f.err != nil {
		errStr = f.err.Error()
	}

	normalizedOutput := f.output
	if f.output != nil {
		normalized, err := normalizeData(f.output)
		if err != nil {
			return nil, fmt.Errorf("failed to normalize finish event output: %w", err)
		}
		normalizedOutput = normalized
	}

	return json.Marshal(map[string]any{
		"output": normalizedOutput,
		"error":  errStr,
	})
}

func (f *FinishEvent) UnmarshalJSON(data []byte) error {
	var aux struct {
		Output any    `json:"output"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	f.output = aux.Output
	if aux.Error != "" {
		f.err = fmt.Errorf("%s", aux.Error)
	}
	return nil
}

// StartEvent metadata
type StartEvent struct {
	scriptHash string
	input      any
}

func NewStartEvent(scriptHash string, input any) StartEvent {
	return StartEvent{scriptHash: scriptHash, input: input}
}

func (s StartEvent) EventType() EventType { return EventTypeStart }
func (s StartEvent) ScriptHash() string   { return s.scriptHash }
func (s StartEvent) Input() any           { return s.input }

func (s StartEvent) MarshalJSON() ([]byte, error) {
	normalizedInput := s.input
	if s.input != nil {
		normalized, err := normalizeData(s.input)
		if err != nil {
			return nil, fmt.Errorf("failed to normalize start event input: %w", err)
		}
		normalizedInput = normalized
	}

	return json.Marshal(map[string]any{
		"scriptHash": s.scriptHash,
		"input":      normalizedInput,
	})
}

func (s *StartEvent) UnmarshalJSON(data []byte) error {
	var aux struct {
		ScriptHash string `json:"scriptHash"`
		Input      any    `json:"input"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	s.scriptHash = aux.ScriptHash
	s.input = aux.Input
	return nil
}

// Event represents a single event in the execution history of a run.
type Event struct {
	Timestamp time.Time
	Metadata  EventMetadata
}

func (e Event) Type() EventType {
	return e.Metadata.EventType()
}

// MarshalJSON serializes the Event to JSON
func (e Event) MarshalJSON() ([]byte, error) {
	// Create a struct that includes the event type for proper deserialization
	return json.Marshal(struct {
		Timestamp time.Time   `json:"timestamp"`
		Type      EventType   `json:"type"`
		Metadata  interface{} `json:"metadata"`
	}{
		Timestamp: e.Timestamp,
		Type:      e.Metadata.EventType(),
		Metadata:  e.Metadata,
	})
}

// UnmarshalJSON deserializes JSON into an Event
func (e *Event) UnmarshalJSON(data []byte) error {
	var aux struct {
		Timestamp time.Time       `json:"timestamp"`
		Type      EventType       `json:"type"`
		Metadata  json.RawMessage `json:"metadata"`
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	e.Timestamp = aux.Timestamp

	// Unmarshal metadata based on type
	switch aux.Type {
	case EventTypeStart:
		var metadata StartEvent
		if err := json.Unmarshal(aux.Metadata, &metadata); err != nil {
			return err
		}
		e.Metadata = metadata
	case EventTypeFinish:
		var metadata FinishEvent
		if err := json.Unmarshal(aux.Metadata, &metadata); err != nil {
			return err
		}
		e.Metadata = metadata
	case EventTypeCall:
		var metadata CallEvent
		if err := json.Unmarshal(aux.Metadata, &metadata); err != nil {
			return err
		}
		e.Metadata = metadata
	case EventTypeReturn:
		var metadata ReturnEvent
		if err := json.Unmarshal(aux.Metadata, &metadata); err != nil {
			return err
		}
		e.Metadata = metadata
	case EventTypeYield:
		var metadata YieldEvent
		if err := json.Unmarshal(aux.Metadata, &metadata); err != nil {
			return err
		}
		e.Metadata = metadata
	case EventTypeResume:
		var metadata ResumeEvent
		if err := json.Unmarshal(aux.Metadata, &metadata); err != nil {
			return err
		}
		e.Metadata = metadata
	case EventTypeSleep:
		var metadata SleepEvent
		if err := json.Unmarshal(aux.Metadata, &metadata); err != nil {
			return err
		}
		e.Metadata = metadata
	case EventTypeTimeNow:
		var metadata TimeNowEvent
		if err := json.Unmarshal(aux.Metadata, &metadata); err != nil {
			return err
		}
		e.Metadata = metadata
	case EventTypeRandInt:
		var metadata RandIntEvent
		if err := json.Unmarshal(aux.Metadata, &metadata); err != nil {
			return err
		}
		e.Metadata = metadata
	default:
		return fmt.Errorf("unknown event type: %s", aux.Type)
	}

	return nil
}

// normalizeData converts data to its JSON-serialized form to ensure consistency
// between recording and replay.
func normalizeData(data any) (any, error) {
	if data == nil {
		return nil, nil
	}

	// Convert to JSON and back to normalize the representation
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal data: %w", err)
	}

	var normalized any
	if err := json.Unmarshal(jsonBytes, &normalized); err != nil {
		return nil, fmt.Errorf("failed to unmarshal data: %w", err)
	}

	return normalized, nil
}
