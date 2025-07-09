package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "embed"

	"github.com/chzyer/readline"
	"github.com/joho/godotenv"
	"github.com/lmittmann/tint"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/responses"

	"github.com/dynoinc/starflow/mcplex"
	"github.com/dynoinc/starflow/ragstore"
	"github.com/dynoinc/starflow/workflow"
	"github.com/dynoinc/starflow/workflow/events"
)

//go:embed assistant.star
var assistantScript []byte

type Input struct {
	ConversationID string `json:"conversation_id"`
	UserID         string `json:"user_id"`
	Message        string `json:"message"`
}

func hook(runID string, version int, event events.Event) {
	switch v := event.Metadata.(type) {
	case events.StartEvent:
		fmt.Printf("[debug] runID: %s, version: %d, START: script=%s\n", runID, version, v.ScriptHash()[:8])
	case events.FinishEvent:
		output, err := v.Output()
		status := "success"
		if err != nil {
			status = "error: " + err.Error()
		}
		fmt.Printf("[debug] runID: %s, version: %d, FINISH: %s (output: %T)\n", runID, version, status, output)
	case events.CallEvent:
		fmt.Printf("[debug] runID: %s, version: %d, CALL: %s (input: %T)\n", runID, version, v.FunctionName(), v.Input())
	case events.ReturnEvent:
		output, err := v.Output()
		status := "success"
		if err != nil {
			status = "error: " + err.Error()
		}
		fmt.Printf("[debug] runID: %s, version: %d, RETURN: %s (output: %T)\n", runID, version, status, output)
	case events.YieldEvent:
		fmt.Printf("[debug] runID: %s, version: %d, YIELD: to runID=%s\n", runID, version, v.RunID())
	case events.ResumeEvent:
		fmt.Printf("[debug] runID: %s, version: %d, RESUME: (output: %T)\n", runID, version, v.Output())
	case events.SleepEvent:
		fmt.Printf("[debug] runID: %s, version: %d, SLEEP: until %s\n", runID, version, v.WakeupAt().Format("15:04:05"))
	case events.TimeNowEvent:
		fmt.Printf("[debug] runID: %s, version: %d, TIME_NOW: %s\n", runID, version, v.Timestamp().Format("15:04:05"))
	case events.RandIntEvent:
		fmt.Printf("[debug] runID: %s, version: %d, RAND_INT: %d\n", runID, version, v.Result())
	default:
		fmt.Printf("[debug] runID: %s, version: %d, UNKNOWN: %T\n", runID, version, v)
	}
}

// TermiChat now uses PostgreSQL via ragstore for conversations and memories.
// Required environment variables:
//   - DATABASE_URL: PostgreSQL connection string (e.g., "postgres://user:pass@localhost/dbname?sslmode=disable")
//   - OPENAI_API_KEY for semantic search capabilities
//   - MCP_CONFIG for MCP servers
//
// and any other environment variables required by the MCP servers.
func main() {
	conversationID := flag.String("conversation", "default", "Conversation ID")
	userID := flag.String("user", "default", "User ID")
	flag.Parse()
	godotenv.Load()
	ctx := context.Background()

	if err := workflow.ValidateScript(assistantScript); err != nil {
		panic(err)
	}

	// Logging
	logHandler := tint.NewHandler(os.Stdout, &tint.Options{
		Level:      slog.LevelInfo,
		TimeFormat: time.StampMilli,
	})
	logger := slog.New(logHandler)
	slog.SetDefault(logger)

	// Database (ragstore for conversations/memories, PostgreSQL for starflow events)
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		panic("DATABASE_URL environment variable is required")
	}

	wfStore, err := workflow.NewStore(ctx, databaseURL, workflow.WithHook(hook))
	if err != nil {
		panic(fmt.Errorf("failed to create workflow store: %w", err))
	}

	// Starflow
	client := workflow.NewClient[Input, string](wfStore)

	// OpenAI client
	openaiClient := openai.NewClient()
	workflow.RegisterFunc(client, func(ctx context.Context, req openai.ChatCompletionNewParams) (*openai.ChatCompletion, error) {
		return openaiClient.Chat.Completions.New(ctx, req)
	}, workflow.WithName("openai.complete"))
	workflow.RegisterFunc(client, func(ctx context.Context, req responses.ResponseNewParams) (*responses.Response, error) {
		return openaiClient.Responses.New(ctx, req)
	}, workflow.WithName("openai.responses"))

	// RagStore for conversations and memories
	ragStore, err := ragstore.NewStore(
		ctx,
		databaseURL,
		ragstore.WithEmbedder(ctx, &openaiClient, "text-embedding-3-small"))
	if err != nil {
		panic(err)
	}
	defer ragStore.Close()

	// RagStore
	workflow.RegisterFunc(client, ragStore.UpsertDocument, workflow.WithName("ragstore.upsert_document"))
	workflow.RegisterFunc(client, ragStore.GetDocuments, workflow.WithName("ragstore.get_documents"))
	workflow.RegisterFunc(client, ragStore.GetDocument, workflow.WithName("ragstore.get_document"))
	workflow.RegisterFunc(client, ragStore.DeleteDocument, workflow.WithName("ragstore.delete_document"))
	workflow.RegisterFunc(client, ragStore.LexicalSearch, workflow.WithName("ragstore.lexical_search"))
	workflow.RegisterFunc(client, ragStore.SemanticSearch, workflow.WithName("ragstore.semantic_search"))

	// MCP
	clients, err := mcplex.Start(ctx, os.Getenv("MCP_CONFIG"))
	if err != nil {
		panic(err)
	}
	workflow.RegisterFunc(client, clients.ListServers, workflow.WithName("mcp.list_servers"))
	workflow.RegisterFunc(client, clients.ListTools, workflow.WithName("mcp.list_tools"))
	workflow.RegisterFunc(client, clients.CallTool, workflow.WithName("mcp.call_tool"))
	workflow.RegisterFunc(client, clients.ListResources, workflow.WithName("mcp.list_resources"))
	workflow.RegisterFunc(client, clients.ReadResource, workflow.WithName("mcp.read_resource"))

	historyFile := filepath.Join(os.TempDir(), "termichat_history.txt")
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "👤 ",
		HistoryFile:     historyFile,
		InterruptPrompt: "^C",
		EOFPrompt:       "Goodbye!\n",
	})
	if err != nil {
		panic(err)
	}
	defer rl.Close()

	appendMsg := func(role, msg string) error {
		_, err = ragStore.UpsertDocument(ctx, ragstore.UserDocument{
			ID:   fmt.Sprintf("conversation-%s-%d", *conversationID, time.Now().UnixNano()),
			Kind: "conversation/" + *conversationID,
			Attributes: map[string]string{
				"conversation_id": *conversationID,
				"role":            role,
			},
			Body:        msg,
			Timestamp:   time.Now(),
			ContentType: "text/plain",
			Source:      "termichat",
		})
		if err != nil {
			return fmt.Errorf("failed to store message: %w", err)
		}

		return nil
	}

	fmt.Printf("Welcome to TermiChat!. (Conversation: %s, User: %s)\n", *conversationID, *userID)
	fmt.Println("Type your message and press Enter. Press Ctrl+C to quit.")
	for {
		msg, err := rl.Readline()
		if err == readline.ErrInterrupt {
			if len(msg) == 0 {
				fmt.Println("Goodbye!")
				break
			}
			continue
		}
		if err == io.EOF {
			break
		}

		if msg == "" {
			continue
		}

		runID := fmt.Sprintf("chat-%d", time.Now().UnixNano())
		fmt.Println("")

		if err := appendMsg("user", msg); err != nil {
			fmt.Println("[error appending user message]", err)
			continue
		}

		input := Input{
			ConversationID: *conversationID,
			UserID:         *userID,
			Message:        msg,
		}

		resp, err := client.Run(ctx, runID, assistantScript, input)
		if err != nil {
			fmt.Println("[error running assistant]", err)
			continue
		}

		if err := appendMsg("assistant", resp); err != nil {
			fmt.Println("[error appending assistant message]", err)
			continue
		}

		fmt.Printf("🤖 %s\n\n", resp)
	}
}
