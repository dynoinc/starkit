package workflow_test

import (
	"context"
	"fmt"
	"log"
	"testing"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/dynoinc/starflow/workflow"
)

func Example() {
	// Set up a store (in a real app, this would be a persistent PostgreSQL instance)
	ctx := context.Background()
	postgresContainer, err := postgres.Run(ctx, "postgres:16-alpine", postgres.BasicWaitStrategies())
	if err != nil {
		log.Fatalf("failed to run postgres container: %v", err)
	}
	defer postgresContainer.Terminate(ctx)
	connStr := postgresContainer.MustConnectionString(ctx, "sslmode=disable")
	store, err := workflow.NewStore(ctx, connStr)
	if err != nil {
		log.Fatalf("failed to create store: %v", err)
	}
	defer store.Close()

	// Create a new client
	client := workflow.NewClient[any, any](store)

	// Register a Go function to be callable from Starlark
	workflow.RegisterFunc(client, func(ctx context.Context, req any) (any, error) {
		return "Hello, " + req.(string), nil
	}, workflow.WithName("example.Greet"))

	// Define the workflow script
	script := `
def main(ctx, input):
    return example.Greet(ctx=ctx, req=input)
`

	// Run the workflow
	output, err := client.Run(ctx, "example-run-1", []byte(script), "World")
	if err != nil {
		log.Fatalf("Workflow failed: %v", err)
	}

	fmt.Println(output)
	// Output: Hello, World
}

func TestExample(t *testing.T) {
	// This test just runs the example to make sure it's valid.
	Example()
}
