package ragstore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// MockEmbedder implements a simple mock for testing embeddings
type MockEmbedder struct{}

func (m *MockEmbedder) CreateEmbeddings(ctx context.Context, input []string) ([][]float64, error) {
	// Create deterministic embeddings based on input text
	embeddings := make([][]float64, len(input))
	for i, text := range input {
		embedding := make([]float64, 1536)
		// Simple hash-like function for deterministic embeddings
		hash := 0
		for _, r := range text {
			hash = hash*31 + int(r)
		}

		for j := range embedding {
			// Create a deterministic but varied embedding
			embedding[j] = float64((hash+j*7)%1000) / 1000.0
		}
		embeddings[i] = embedding
	}
	return embeddings, nil
}

func setupTestDB(t *testing.T) *Store {
	t.Helper()

	ctx := t.Context()
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	postgresContainer, err := postgres.Run(ctx, "pgvector/pgvector:pg16", postgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { _ = postgresContainer.Terminate(context.Background()) })

	store, err := NewStore(ctx, postgresContainer.MustConnectionString(ctx, "sslmode=disable"))
	require.NoError(t, err)
	t.Cleanup(store.Close)
	return store
}

func TestStore_CRUD(t *testing.T) {
	store := setupTestDB(t)

	ctx := t.Context()
	doc := UserDocument{
		ID:          "doc1",
		Source:      "test",
		Kind:        "testkind",
		ContentType: "text/plain",
		Attributes:  map[string]string{"foo": "bar"},
		Body:        "chunk1\nchunk2\nchunk3",
		Timestamp:   time.Now(),
	}

	// Upsert
	_, err := store.UpsertDocument(ctx, doc)
	require.NoError(t, err)

	// Get
	retrieved, err := store.GetDocument(ctx, "doc1")
	require.NoError(t, err)
	assert.Equal(t, doc.ID, retrieved.ID)
	assert.Equal(t, doc.Body, retrieved.Body)
	assert.Equal(t, doc.Attributes["foo"], retrieved.Attributes["foo"])

	// Delete
	_, err = store.DeleteDocument(ctx, "doc1")
	require.NoError(t, err)

	_, err = store.GetDocument(ctx, "doc1")
	assert.Error(t, err)
}

func TestStore_CRUD_UpdateDocument(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	doc := UserDocument{
		ID:          "doc1",
		Source:      "original",
		Kind:        "test",
		ContentType: "text/plain",
		Attributes:  map[string]string{"version": "1"},
		Body:        "original content",
		Timestamp:   time.Now(),
	}

	// Initial insert
	_, err := store.UpsertDocument(ctx, doc)
	require.NoError(t, err)

	// Update the document
	updatedDoc := doc
	updatedDoc.Source = "updated"
	updatedDoc.Attributes = map[string]string{"version": "2", "updated": "true"}
	updatedDoc.Body = "updated content"
	updatedDoc.Timestamp = time.Now()

	_, err = store.UpsertDocument(ctx, updatedDoc)
	require.NoError(t, err)

	// Verify update
	retrieved, err := store.GetDocument(ctx, "doc1")
	require.NoError(t, err)
	assert.Equal(t, "updated", retrieved.Source)
	assert.Equal(t, "updated content", retrieved.Body)
	assert.Equal(t, "2", retrieved.Attributes["version"])
	assert.Equal(t, "true", retrieved.Attributes["updated"])
}

func TestStore_LexicalSearch(t *testing.T) {
	store := setupTestDB(t)

	ctx := t.Context()
	baseTime := time.Now()
	doc1 := UserDocument{
		ID:          "doc1",
		Source:      "test",
		Kind:        "kind/a",
		ContentType: "text/plain",
		Attributes:  map[string]string{"author": "rajat"},
		Body:        "this is a test document",
		Timestamp:   baseTime.Add(-2 * time.Hour), // Clearly older
	}
	doc2 := UserDocument{
		ID:          "doc2",
		Source:      "test",
		Kind:        "kind/b",
		ContentType: "text/plain",
		Attributes:  map[string]string{"author": "other"},
		Body:        "another test document about starkit",
		Timestamp:   baseTime.Add(-1 * time.Hour), // Newer than doc1
	}
	_, err := store.UpsertDocument(ctx, doc1)
	require.NoError(t, err)
	_, err = store.UpsertDocument(ctx, doc2)
	require.NoError(t, err)

	t.Run("search all", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "test", Limit: 10})
		require.NoError(t, err)
		assert.Len(t, results, 2)
		// Ensure all chunks are returned
		if results[0].DocumentID == "doc1" {
			assert.Len(t, results[0].Chunks, 1)
			assert.Len(t, results[1].Chunks, 1)
		} else {
			assert.Len(t, results[0].Chunks, 1)
			assert.Len(t, results[1].Chunks, 1)
		}
	})

	t.Run("search with kind filter", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "test", KindPrefix: "kind/a", Limit: 10})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "doc1", results[0].DocumentID)
		assert.Len(t, results[0].Chunks, 1)
	})

	t.Run("search with attribute filter", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "starkit", Filters: map[string]string{"author": "other"}, Limit: 10})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "doc2", results[0].DocumentID)
		assert.Len(t, results[0].Chunks, 1)
	})

	t.Run("search with ordering", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "document", Limit: 10})
		require.NoError(t, err)
		assert.Len(t, results, 2)
		// Since we removed OrderBy support, lexical search now returns by score descending
		// So we just verify both documents are returned
		docIDs := []string{results[0].DocumentID, results[1].DocumentID}
		assert.Contains(t, docIDs, "doc1")
		assert.Contains(t, docIDs, "doc2")
	})
}

func TestStore_LexicalSearch_ComplexAttributes(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	// Documents with complex JSON attributes
	docs := []UserDocument{
		{
			ID:          "doc1",
			ContentType: "text/plain",
			Attributes: map[string]string{
				"category": "technical",
				"priority": "high",
				"tags":     "golang,database",
				"version":  "1.0",
			},
			Body:      "technical documentation about golang databases",
			Timestamp: time.Now(),
		},
		{
			ID:          "doc2",
			ContentType: "text/plain",
			Attributes: map[string]string{
				"category": "business",
				"priority": "medium",
				"tags":     "planning,strategy",
			},
			Body:      "business planning documentation",
			Timestamp: time.Now(),
		},
	}

	for _, doc := range docs {
		_, err := store.UpsertDocument(ctx, doc)
		require.NoError(t, err)
	}

	t.Run("filter by category", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{
			KeywordQuery: "documentation",
			Filters:      map[string]string{"category": "technical"},
			Limit:        10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "doc1", results[0].DocumentID)
	})

	t.Run("filter by priority", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{
			KeywordQuery: "documentation",
			Filters:      map[string]string{"priority": "high"},
			Limit:        10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "doc1", results[0].DocumentID)
	})
}

func TestStore_LexicalSearch_MultipleMatchesInDoc(t *testing.T) {
	store := setupTestDB(t)

	ctx := t.Context()
	var body string
	for i := 0; i < 5; i++ {
		body += fmt.Sprintf("chunk %d is about starkit\n", i)
	}

	doc := UserDocument{
		ID:          "doc-multi-match",
		ContentType: "text/plain",
		Body:        strings.TrimSuffix(body, "\n"),
	}
	_, err := store.UpsertDocument(ctx, doc)
	require.NoError(t, err)

	results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "starkit"})
	require.NoError(t, err)

	assert.Len(t, results, 1)
	assert.Equal(t, "doc-multi-match", results[0].DocumentID)
	assert.Len(t, results[0].Chunks, 5)
	for i := 0; i < 5; i++ {
		assert.Contains(t, results[0].Chunks[i], fmt.Sprintf("chunk %d", i))
	}
}

func TestStore_LexicalSearch_FullTextFeatures(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	docs := []UserDocument{
		{
			ID:          "doc1",
			Body:        "PostgreSQL is a powerful relational database management system.",
			ContentType: "text/plain",
			Timestamp:   time.Now(),
		},
		{
			ID:          "doc2",
			Body:        "pgvector provides vector similarity search for PostgreSQL databases.",
			ContentType: "text/plain",
			Timestamp:   time.Now(),
		},
		{
			ID:          "doc3",
			Body:        "Full text search capabilities in modern database systems.",
			ContentType: "text/plain",
			Timestamp:   time.Now(),
		},
	}

	for _, doc := range docs {
		_, err := store.UpsertDocument(ctx, doc)
		require.NoError(t, err)
	}

	t.Run("exact word search", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "PostgreSQL", Limit: 10})
		require.NoError(t, err)
		assert.Len(t, results, 2) // Should find both docs mentioning PostgreSQL
	})

	t.Run("phrase search", func(t *testing.T) {
		// plainto_tsquery doesn't support phrase search well, so we search for words
		results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "database management", Limit: 10})
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(results), 1)
	})

	t.Run("stemming search", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "databases", Limit: 10})
		require.NoError(t, err)
		assert.Len(t, results, 3) // Should find "database" and "databases"
	})
}

func TestStore_SemanticSearch_NoEmbedder(t *testing.T) {
	store := setupTestDB(t)

	ctx := t.Context()
	_, err := store.SemanticSearch(ctx, Query{SemanticQuery: "test"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "embedder not configured")
}

func TestStore_ConcurrentOperations(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	// Test concurrent document insertions
	const numGoroutines = 10
	const docsPerGoroutine = 5

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*docsPerGoroutine)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(routineID int) {
			defer wg.Done()
			for j := 0; j < docsPerGoroutine; j++ {
				doc := UserDocument{
					ID:          fmt.Sprintf("doc-%d-%d", routineID, j),
					Source:      fmt.Sprintf("routine-%d", routineID),
					ContentType: "text/plain",
					Attributes:  map[string]string{"routine": fmt.Sprintf("%d", routineID), "index": fmt.Sprintf("%d", j)},
					Body:        fmt.Sprintf("Document %d from routine %d", j, routineID),
					Timestamp:   time.Now(),
				}
				if _, err := store.UpsertDocument(ctx, doc); err != nil {
					errors <- err
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	// Check for errors
	for err := range errors {
		t.Errorf("Concurrent operation failed: %v", err)
	}

	// Verify all documents were inserted
	results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "Document", Limit: 100})
	require.NoError(t, err)
	assert.Equal(t, numGoroutines*docsPerGoroutine, len(results))
}

func TestStore_LargeDataset(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping large dataset test in short mode")
	}

	store := setupTestDB(t)
	ctx := t.Context()

	// Insert a larger number of documents
	const numDocs = 100
	docs := make([]UserDocument, numDocs)

	for i := 0; i < numDocs; i++ {
		docs[i] = UserDocument{
			ID:          fmt.Sprintf("large-doc-%d", i),
			Source:      "performance-test",
			Kind:        fmt.Sprintf("category-%d", i%10), // 10 different categories
			ContentType: "text/plain",
			Attributes: map[string]string{
				"index":    fmt.Sprintf("%d", i),
				"category": fmt.Sprintf("%d", i%10),
				"batch":    fmt.Sprintf("%d", i/10),
			},
			Body:      fmt.Sprintf("This is document number %d with some searchable content about topic %d", i, i%5),
			Timestamp: time.Now().Add(time.Duration(i) * time.Minute),
		}
	}

	start := time.Now()
	for _, doc := range docs {
		_, err := store.UpsertDocument(ctx, doc)
		require.NoError(t, err)
	}
	insertDuration := time.Since(start)
	t.Logf("Inserted %d documents in %v", numDocs, insertDuration)

	// Test search performance
	start = time.Now()
	results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "document", Limit: 50})
	require.NoError(t, err)
	searchDuration := time.Since(start)
	t.Logf("Search returned %d results in %v", len(results), searchDuration)

	assert.GreaterOrEqual(t, len(results), 50)
	assert.LessOrEqual(t, len(results), 50) // Should respect limit

	// Test filtering performance
	start = time.Now()
	results, err = store.LexicalSearch(ctx, Query{
		KeywordQuery: "content",
		KindPrefix:   "category-5",
		Limit:        20,
	})
	require.NoError(t, err)
	filterDuration := time.Since(start)
	t.Logf("Filtered search returned %d results in %v", len(results), filterDuration)

	// Verify all results match the filter
	for _, result := range results {
		assert.Equal(t, "category-5", result.Kind)
	}
}

func TestStore_ErrorHandling(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	t.Run("get non-existent document", func(t *testing.T) {
		_, err := store.GetDocument(ctx, "non-existent")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "document not found")
	})

	t.Run("delete non-existent document", func(t *testing.T) {
		_, err := store.DeleteDocument(ctx, "non-existent")
		assert.NoError(t, err) // DELETE should be idempotent
	})

	t.Run("search with empty query", func(t *testing.T) {
		_, err := store.LexicalSearch(ctx, Query{KeywordQuery: ""})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "keyword query is required")
	})

	t.Run("semantic search with empty query", func(t *testing.T) {
		_, err := store.SemanticSearch(ctx, Query{SemanticQuery: ""})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "semantic query is required")
	})
}

func TestStore_AttributeTypes(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	// Test string-only attributes
	doc := UserDocument{
		ID:          "attr-test",
		ContentType: "text/plain",
		Attributes: map[string]string{
			"string_val": "hello world",
			"int_val":    "42",
			"float_val":  "3.14159",
			"bool_val":   "true",
			"empty_val":  "",
			"array_val":  "a,b,c",
			"json_val":   `{"nested_key":"nested_value","nested_num":"123"}`,
		},
		Body:      "Document with string-only attributes",
		Timestamp: time.Now(),
	}

	_, err := store.UpsertDocument(ctx, doc)
	require.NoError(t, err)

	retrieved, err := store.GetDocument(ctx, "attr-test")
	require.NoError(t, err)

	assert.Equal(t, "hello world", retrieved.Attributes["string_val"])
	assert.Equal(t, "42", retrieved.Attributes["int_val"])
	assert.Equal(t, "3.14159", retrieved.Attributes["float_val"])
	assert.Equal(t, "true", retrieved.Attributes["bool_val"])
	assert.Equal(t, "", retrieved.Attributes["empty_val"])
	assert.Equal(t, "a,b,c", retrieved.Attributes["array_val"])
	assert.Equal(t, `{"nested_key":"nested_value","nested_num":"123"}`, retrieved.Attributes["json_val"])
}

func TestStore_DocumentSplitting(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	t.Run("plain text splitter", func(t *testing.T) {
		doc := UserDocument{
			ID:          "split-plain",
			ContentType: "text/plain",
			Body:        "Line 1\nLine 2\nLine 3\nLine 4",
			Timestamp:   time.Now(),
		}
		_, err := store.UpsertDocument(ctx, doc)
		require.NoError(t, err)

		retrieved, err := store.GetDocument(ctx, "split-plain")
		require.NoError(t, err)
		assert.Equal(t, doc.Body, retrieved.Body)

		// Search should find individual chunks
		results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "Line", Limit: 10})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Len(t, results[0].Chunks, 4) // Should have 4 chunks
	})

	t.Run("markdown splitter", func(t *testing.T) {
		doc := UserDocument{
			ID:          "split-markdown",
			ContentType: "text/markdown",
			Body:        "# Header 1\n\nParagraph 1 content.\n\n## Header 2\n\nParagraph 2 content.",
			Timestamp:   time.Now(),
		}
		_, err := store.UpsertDocument(ctx, doc)
		require.NoError(t, err)

		results, err := store.LexicalSearch(ctx, Query{KeywordQuery: "content", Limit: 10})
		require.NoError(t, err)

		// Find the markdown document
		var mdResult *SearchResult
		for _, result := range results {
			if result.DocumentID == "split-markdown" {
				mdResult = &result
				break
			}
		}
		require.NotNil(t, mdResult)
		assert.GreaterOrEqual(t, len(mdResult.Chunks), 2) // Should have multiple chunks split by paragraphs
	})
}

func TestStore_GetDocuments(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	baseTime := time.Now()
	docs := []UserDocument{
		{
			ID:          "doc1",
			Source:      "test",
			Kind:        "conversation",
			ContentType: "text/plain",
			Attributes: map[string]string{
				"conversation_id": "conv1",
				"role":            "user",
			},
			Body:      "Hello world",
			Timestamp: baseTime.Add(-2 * time.Hour), // Older
		},
		{
			ID:          "doc2",
			Source:      "test",
			Kind:        "conversation",
			ContentType: "text/plain",
			Attributes: map[string]string{
				"conversation_id": "conv1",
				"role":            "assistant",
			},
			Body:      "Hi there!",
			Timestamp: baseTime.Add(-1 * time.Hour), // Newer
		},
		{
			ID:          "doc3",
			Source:      "test",
			Kind:        "memory",
			ContentType: "text/plain",
			Attributes:  map[string]string{},
			Body:        "Remember this important fact",
			Timestamp:   baseTime.Add(-30 * time.Minute), // Newest
		},
		{
			ID:          "doc4",
			Source:      "test",
			Kind:        "conversation",
			ContentType: "text/plain",
			Attributes: map[string]string{
				"conversation_id": "conv2",
				"role":            "user",
			},
			Body:      "Different conversation",
			Timestamp: baseTime.Add(-3 * time.Hour), // Oldest
		},
	}

	// Insert all documents
	for _, doc := range docs {
		_, err := store.UpsertDocument(ctx, doc)
		require.NoError(t, err)
	}

	t.Run("get all conversations", func(t *testing.T) {
		results, err := store.GetDocuments(ctx, Query{
			KindPrefix: "conversation",
			Limit:      10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 3)
		// Should be ordered by timestamp DESC (newest first)
		assert.Equal(t, "doc2", results[0].ID)
		assert.Equal(t, "doc1", results[1].ID)
		assert.Equal(t, "doc4", results[2].ID)
	})

	t.Run("get conversations for specific conversation_id", func(t *testing.T) {
		results, err := store.GetDocuments(ctx, Query{
			KindPrefix: "conversation",
			Filters: map[string]string{
				"conversation_id": "conv1",
			},
			Limit: 10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 2)
		// Should have both messages from conv1
		assert.Equal(t, "doc2", results[0].ID) // assistant (newer)
		assert.Equal(t, "doc1", results[1].ID) // user (older)
	})

	t.Run("get memories", func(t *testing.T) {
		results, err := store.GetDocuments(ctx, Query{
			KindPrefix: "memory",
			Limit:      10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "doc3", results[0].ID)
		assert.Equal(t, "Remember this important fact", results[0].Body)
	})

	t.Run("get with limit", func(t *testing.T) {
		results, err := store.GetDocuments(ctx, Query{
			KindPrefix: "conversation",
			Limit:      2,
		})
		require.NoError(t, err)
		assert.Len(t, results, 2)
		// Should get the 2 newest conversations
		assert.Equal(t, "doc2", results[0].ID)
		assert.Equal(t, "doc1", results[1].ID)
	})

	t.Run("get with timestamp ordering", func(t *testing.T) {
		results, err := store.GetDocuments(ctx, Query{
			KindPrefix: "conversation",
			Limit:      10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 3)
		// Should be ordered by timestamp DESC (newest first) since OrderBy was removed
		assert.Equal(t, "doc2", results[0].ID) // newest
		assert.Equal(t, "doc1", results[1].ID)
		assert.Equal(t, "doc4", results[2].ID) // oldest
	})

	t.Run("get with multiple filters", func(t *testing.T) {
		results, err := store.GetDocuments(ctx, Query{
			KindPrefix: "conversation",
			Filters: map[string]string{
				"conversation_id": "conv1",
				"role":            "user",
			},
			Limit: 10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "doc1", results[0].ID)
		assert.Equal(t, "user", results[0].Attributes["role"])
	})

	t.Run("get non-existent kind", func(t *testing.T) {
		results, err := store.GetDocuments(ctx, Query{
			KindPrefix: "nonexistent",
			Limit:      10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 0)
	})
}

func TestStore_LexicalSearchResults(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	baseTime := time.Now()
	doc1 := UserDocument{
		ID:          "doc1",
		Source:      "test",
		Kind:        "kind/a",
		ContentType: "text/plain",
		Attributes:  map[string]string{"author": "rajat"},
		Body:        "this is a test document",
		Timestamp:   baseTime.Add(-2 * time.Hour),
	}
	doc2 := UserDocument{
		ID:          "doc2",
		Source:      "test",
		Kind:        "kind/b",
		ContentType: "text/plain",
		Attributes:  map[string]string{"author": "other"},
		Body:        "another test document about starkit",
		Timestamp:   baseTime.Add(-1 * time.Hour),
	}

	_, err := store.UpsertDocument(ctx, doc1)
	require.NoError(t, err)
	_, err = store.UpsertDocument(ctx, doc2)
	require.NoError(t, err)

	t.Run("search all test documents", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{
			KeywordQuery: "test",
			Limit:        10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 2)

		// Verify results have expected fields
		for _, result := range results {
			assert.NotEmpty(t, result.DocumentID)
			assert.NotEmpty(t, result.Chunks)
			assert.GreaterOrEqual(t, result.Score, 0.0)
		}
	})

	t.Run("search with kind filter", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{
			KeywordQuery: "test",
			KindPrefix:   "kind/a",
			Limit:        10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "doc1", results[0].DocumentID)
	})

	t.Run("search with attribute filter", func(t *testing.T) {
		results, err := store.LexicalSearch(ctx, Query{
			KeywordQuery: "starkit",
			Filters:      map[string]string{"author": "other"},
			Limit:        10,
		})
		require.NoError(t, err)
		assert.Len(t, results, 1)
		assert.Equal(t, "doc2", results[0].DocumentID)
	})
}

func TestStore_SemanticSearchResults_NoEmbedder(t *testing.T) {
	store := setupTestDB(t)
	ctx := t.Context()

	_, err := store.SemanticSearch(ctx, Query{
		SemanticQuery: "test query",
		Limit:         10,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "embedder not configured")
}

func TestStore_UnlimitedAttributeFiltering(t *testing.T) {
	store := setupTestDB(t)
	defer store.Close()

	// Insert a document with many attributes
	doc := UserDocument{
		ID:        "doc1",
		Kind:      "test",
		Body:      "This is a test document",
		Timestamp: time.Now(),
		Attributes: map[string]string{
			"attr1":  "value1",
			"attr2":  "value2",
			"attr3":  "value3",
			"attr4":  "value4",
			"attr5":  "value5",
			"attr6":  "value6",
			"attr7":  "value7",
			"attr8":  "value8",
			"attr9":  "value9",
			"attr10": "value10",
		},
	}

	_, err := store.UpsertDocument(context.Background(), doc)
	require.NoError(t, err)

	// Test filtering with many attributes (more than the old 3-filter limit)
	query := Query{
		Filters: map[string]string{
			"attr1": "value1",
			"attr3": "value3",
			"attr5": "value5",
			"attr7": "value7",
			"attr9": "value9",
		},
		Limit: 10,
	}

	docs, err := store.GetDocuments(context.Background(), query)
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, "doc1", docs[0].ID)

	// Test that partial matches don't work (all filters must match)
	query.Filters["attr1"] = "wrong_value"
	docs, err = store.GetDocuments(context.Background(), query)
	require.NoError(t, err)
	require.Len(t, docs, 0)

	// Test lexical search with many filters
	query.Filters["attr1"] = "value1" // Fix the filter
	query.KeywordQuery = "test"

	results, err := store.LexicalSearch(context.Background(), query)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "doc1", results[0].DocumentID)
}
