package main

import (
	"testing"

	"github.com/dynoinc/starflow/workflow"
)

func TestAssistantScriptValidation(t *testing.T) {
	if err := workflow.ValidateScript(assistantScript); err != nil {
		t.Fatalf("Assistant script validation failed: %v", err)
	}
}
