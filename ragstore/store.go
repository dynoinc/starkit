package ragstore

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/openai/openai-go"
	pgvector "github.com/pgvector/pgvector-go"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// UserDocument represents a document to be stored (user-facing API).
type UserDocument struct {
	ID          string            `json:"id"`           // User-defined unique ID
	Source      string            `json:"source"`       // Optional: Source URL
	Kind        string            `json:"kind"`         // Optional: Hierarchical kind (e.g., "channel/thread")
	ContentType string            `json:"content_type"` // Optional: MIME type like "text/plain", "text/markdown"
	Attributes  map[string]string `json:"attributes"`   // Optional: Key-value metadata (string values only)
	Body        string            `json:"body"`         // The document content
	Timestamp   time.Time         `json:"timestamp"`    // The document's timestamp
}

// SearchResult represents a single search result chunk.
type SearchResult struct {
	DocumentID string            `json:"document_id"` // The ID of the document this chunk belongs to
	Chunks     []string          `json:"chunks"`      // The text of the result chunks, merged
	Score      float64           `json:"score"`       // Relevance score
	Timestamp  time.Time         `json:"timestamp"`   // Document timestamp
	Source     string            `json:"source"`      // Document source
	Kind       string            `json:"kind"`        // Document kind
	Attributes map[string]string `json:"attributes"`  // Document attributes
}

// rawSearchResult is an intermediate struct to hold results from the database before merging.
type rawSearchResult struct {
	DocumentID string
	Chunk      string
	ChunkIndex int64
	Score      float64
	Timestamp  time.Time
	Source     string
	Kind       string
	Attributes map[string]string
}

// Query represents a search query.
type Query struct {
	// Filters
	KindPrefix string            `json:"kind_prefix"` // Filter by kind prefix (e.g., "channel/")
	Filters    map[string]string `json:"filters"`     // Filter by exact match on attributes

	// Search
	SemanticQuery string `json:"semantic_query"` // Text for semantic search
	KeywordQuery  string `json:"keyword_query"`  // Text for keyword (FTS) search

	// Limits
	Limit int `json:"limit"` // Number of results to return
}

const embeddingDimension = 1536

// convertFiltersToJSONB converts a map[string]string into a JSONB byte array for use with the @> operator.
// This allows filtering on any number of attributes at once.
func convertFiltersToJSONB(filters map[string]string) ([]byte, error) {
	if len(filters) == 0 {
		return nil, nil
	}

	jsonBytes, err := json.Marshal(filters)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal filters to JSON: %w", err)
	}

	return jsonBytes, nil
}

// Splitter is a function that splits content into chunks.
type Splitter func(content string) []string

// Embedder is a function that embeds text into a vector.
type Embedder func(ctx context.Context, texts []string) (int32, []pgvector.Vector, error)

// Store implements the RAGStore interface.
type Store struct {
	p *pgxpool.Pool
	q *Queries

	splitters map[string]Splitter
	embedder  Embedder
}

// Option is a functional option for configuring the Store.
type Option func(*Store) error

// WithSplitter sets a splitter for a given content type.
func WithSplitter(contentType string, splitter Splitter) Option {
	return func(s *Store) error {
		if s.splitters == nil {
			s.splitters = make(map[string]Splitter)
		}
		s.splitters[contentType] = splitter
		return nil
	}
}

// WithEmbedder configures the embedding client and model.
func WithEmbedder(ctx context.Context, client *openai.Client, model string) Option {
	return func(s *Store) error {
		modelID, err := s.q.CreateEmbeddingModel(ctx, CreateEmbeddingModelParams{
			Name:       model,
			Dimensions: embeddingDimension,
		})
		if err != nil {
			return fmt.Errorf("create embedding model: %w", err)
		}

		s.embedder = func(ctx context.Context, texts []string) (int32, []pgvector.Vector, error) {
			resp, err := client.Embeddings.New(ctx, openai.EmbeddingNewParams{
				Input:          openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: texts},
				Model:          model,
				EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
				Dimensions:     openai.Opt[int64](embeddingDimension),
			})
			if err != nil {
				return 0, nil, fmt.Errorf("create embeddings: %w", err)
			}

			embeddings := make([]pgvector.Vector, len(resp.Data))
			for i, data := range resp.Data {
				embedding64 := data.Embedding
				embedding32 := make([]float32, len(embedding64))
				for j, v := range embedding64 {
					embedding32[j] = float32(v)
				}

				embeddings[i] = pgvector.NewVector(embedding32)
			}

			return modelID, embeddings, nil
		}

		return nil
	}
}

// NewStore returns a new RAGStore for the given PostgreSQL DSN.
func NewStore(ctx context.Context, dsn string, options ...Option) (*Store, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	// Register pgvector types
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// This is where we'd register pgvector types if needed
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Test the connection
	if err := pool.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	if err := Migrate(context.Background(), pool); err != nil {
		return nil, fmt.Errorf("failed to migrate db: %w", err)
	}

	store := &Store{
		p: pool,
		q: New(pool),
		splitters: map[string]Splitter{
			"text/plain": func(content string) []string {
				return strings.Split(content, "\n")
			},
			"text/markdown": func(content string) []string {
				// Trivial markdown splitter inspired by langchain-go: split by double newlines (paragraphs)
				return strings.Split(content, "\n\n")
			},
		},
	}

	for _, opt := range options {
		if err := opt(store); err != nil {
			return nil, err
		}
	}

	return store, nil
}

func Migrate(ctx context.Context, p *pgxpool.Pool) error {
	// Enable pgvector extension
	if _, err := p.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		return fmt.Errorf("failed to enable vector extension: %w", err)
	}

	// Run embedded migrations
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

// UpsertDocumentResponse represents the response from upserting a document.
type UpsertDocumentResponse struct {
	Success bool   `json:"success"`
	ID      string `json:"id"`
}

func (s *Store) UpsertDocument(ctx context.Context, doc UserDocument) (UpsertDocumentResponse, error) {
	tx, err := s.p.Begin(ctx)
	if err != nil {
		return UpsertDocumentResponse{}, err
	}
	defer tx.Rollback(ctx)

	attrs, err := json.Marshal(doc.Attributes)
	if err != nil {
		return UpsertDocumentResponse{}, fmt.Errorf("failed to marshal attributes: %w", err)
	}

	txQueries := s.q.WithTx(tx)
	docDB, err := txQueries.UpsertDocument(ctx, UpsertDocumentParams{
		ExternalID:  doc.ID,
		Source:      doc.Source,
		Kind:        doc.Kind,
		ContentType: doc.ContentType,
		Attributes:  attrs,
		Body:        doc.Body,
		UpdatedAt:   pgtype.Timestamp{Time: doc.Timestamp, Valid: true},
	})

	if err != nil {
		return UpsertDocumentResponse{}, fmt.Errorf("failed to upsert document: %w", err)
	}

	err = txQueries.DeleteChunksByDocumentID(ctx, docDB)
	if err != nil {
		return UpsertDocumentResponse{}, fmt.Errorf("failed to delete old chunks: %w", err)
	}

	var chunks []string
	if doc.Body != "" {
		splitter, ok := s.splitters[doc.ContentType]
		if !ok {
			splitter = s.splitters["text/plain"]
		}

		chunks = splitter(doc.Body)
		chunks = slices.DeleteFunc(chunks, func(chunk string) bool {
			return chunk == ""
		})
	}

	if len(chunks) == 0 {
		err := tx.Commit(ctx)
		return UpsertDocumentResponse{Success: err == nil, ID: doc.ID}, err
	}

	if s.embedder != nil {
		modelID, embeddings, err := s.embedder(ctx, chunks)
		if err != nil {
			return UpsertDocumentResponse{}, err
		}

		for i, embedding := range embeddings {
			if err := txQueries.InsertChunkWithEmbedding(ctx, InsertChunkWithEmbeddingParams{
				DocumentID:       docDB,
				ChunkIndex:       int32(i),
				EmbeddingModelID: pgtype.Int4{Int32: modelID, Valid: true},
				ChunkText:        chunks[i],
				Embedding:        embedding,
			}); err != nil {
				return UpsertDocumentResponse{}, fmt.Errorf("insert chunk with embedding: %w", err)
			}
		}
	} else {
		for i, chunk := range chunks {
			err = txQueries.InsertChunk(ctx, InsertChunkParams{
				DocumentID: docDB,
				ChunkIndex: int32(i),
				ChunkText:  chunk,
			})
			if err != nil {
				return UpsertDocumentResponse{}, fmt.Errorf("failed to insert chunk: %w", err)
			}
		}
	}

	err = tx.Commit(ctx)
	return UpsertDocumentResponse{Success: err == nil, ID: doc.ID}, err
}

func (s *Store) LexicalSearch(ctx context.Context, q Query) ([]SearchResult, error) {
	if q.KeywordQuery == "" {
		return nil, fmt.Errorf("keyword query is required for lexical search")
	}

	// Convert filters to JSONB
	filtersJSONB, err := convertFiltersToJSONB(q.Filters)
	if err != nil {
		return nil, fmt.Errorf("failed to convert filters to JSONB: %w", err)
	}

	limit := q.Limit
	if limit == 0 {
		limit = 10 // Default limit
	}

	searchParams := LexicalSearchParams{
		KeywordQuery: q.KeywordQuery,
		KindPrefix:   q.KindPrefix,
		FiltersJsonb: filtersJSONB,
		LimitCount:   int32(limit),
	}

	rows, err := s.q.LexicalSearch(ctx, searchParams)
	if err != nil {
		return nil, fmt.Errorf("failed to execute search query: %w", err)
	}

	// Convert sqlc rows to rawSearchResult slice
	var raws []rawSearchResult
	for _, row := range rows {
		var attrs map[string]string
		if err := json.Unmarshal(row.Attributes, &attrs); err != nil {
			return nil, fmt.Errorf("failed to unmarshal attributes: %w", err)
		}

		raws = append(raws, rawSearchResult{
			DocumentID: row.ExternalID,
			Chunk:      row.ChunkText,
			ChunkIndex: int64(row.ChunkIndex),
			Score:      float64(row.Score),
			Timestamp:  row.UpdatedAt.Time,
			Source:     row.Source,
			Kind:       row.Kind,
			Attributes: attrs,
		})
	}

	return s.buildSearchResults(raws), nil
}

// SemanticSearch performs a semantic search over the document chunks.
func (s *Store) SemanticSearch(ctx context.Context, q Query) ([]SearchResult, error) {
	if q.SemanticQuery == "" {
		return nil, fmt.Errorf("semantic query is required for semantic search")
	}
	if s.embedder == nil {
		return nil, fmt.Errorf("embedder not configured for semantic search")
	}

	_, queryEmbedding, err := s.embedder(ctx, []string{q.SemanticQuery})
	if err != nil {
		return nil, fmt.Errorf("create query embedding: %w", err)
	}

	// Convert filters to JSONB
	filtersJSONB, err := convertFiltersToJSONB(q.Filters)
	if err != nil {
		return nil, fmt.Errorf("failed to convert filters to JSONB: %w", err)
	}

	limit := q.Limit
	if limit == 0 {
		limit = 10 // Default limit
	}

	searchParams := SemanticSearchParams{
		Embedding:    queryEmbedding[0],
		KindPrefix:   q.KindPrefix,
		FiltersJsonb: filtersJSONB,
		LimitCount:   int32(limit),
	}

	rows, err := s.q.SemanticSearch(ctx, searchParams)
	if err != nil {
		return nil, fmt.Errorf("failed to execute search query: %w", err)
	}

	// Convert sqlc rows to rawSearchResult slice
	var raws []rawSearchResult
	for _, row := range rows {
		var attrs map[string]string
		if err := json.Unmarshal(row.Attributes, &attrs); err != nil {
			return nil, fmt.Errorf("failed to unmarshal attributes: %w", err)
		}

		r, ok := row.Score.(float64)
		if !ok {
			// pgvector distance can come as float32, handle that
			switch v := row.Score.(type) {
			case float32:
				r = float64(v)
			default:
				return nil, fmt.Errorf("unexpected score type %T", row.Score)
			}
		}

		raws = append(raws, rawSearchResult{
			DocumentID: row.ExternalID,
			Chunk:      row.ChunkText,
			ChunkIndex: int64(row.ChunkIndex),
			Score:      r,
			Timestamp:  row.UpdatedAt.Time,
			Source:     row.Source,
			Kind:       row.Kind,
			Attributes: attrs,
		})
	}

	return s.buildSearchResults(raws), nil
}

func (s *Store) GetDocument(ctx context.Context, id string) (UserDocument, error) {
	row, err := s.q.GetDocument(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
			return UserDocument{}, fmt.Errorf("document not found: %w", err)
		}
		return UserDocument{}, fmt.Errorf("failed to get document: %w", err)
	}

	var doc UserDocument
	doc.ID = row.ExternalID
	doc.Source = row.Source
	doc.Kind = row.Kind
	doc.ContentType = row.ContentType
	doc.Body = row.Body
	doc.Timestamp = row.UpdatedAt.Time

	if err := json.Unmarshal(row.Attributes, &doc.Attributes); err != nil {
		return UserDocument{}, fmt.Errorf("failed to unmarshal attributes: %w", err)
	}
	return doc, nil
}

// DeleteDocumentResponse represents the response from deleting a document.
type DeleteDocumentResponse struct {
	Success bool   `json:"success"`
	ID      string `json:"id"`
}

func (s *Store) DeleteDocument(ctx context.Context, id string) (DeleteDocumentResponse, error) {
	err := s.q.DeleteDocument(ctx, id)
	return DeleteDocumentResponse{Success: err == nil, ID: id}, err
}

// DeleteResult represents the result of a delete operation.
type DeleteResult struct {
	Success bool  `json:"success"`
	Count   int64 `json:"count"`
}

// DeleteDocumentsByKindPrefix deletes all documents matching a kind prefix.
func (s *Store) DeleteDocumentsByKindPrefix(ctx context.Context, kindPrefix string) (DeleteResult, error) {
	rowsAffected, err := s.q.DeleteDocumentsByKindPrefix(ctx, kindPrefix)
	if err != nil {
		return DeleteResult{}, fmt.Errorf("failed to delete documents by kind prefix: %w", err)
	}
	return DeleteResult{Success: true, Count: rowsAffected}, nil
}

func (s *Store) GetDocuments(ctx context.Context, q Query) ([]UserDocument, error) {
	// Convert filters to JSONB
	filtersJSONB, err := convertFiltersToJSONB(q.Filters)
	if err != nil {
		return nil, fmt.Errorf("failed to convert filters to JSONB: %w", err)
	}

	limit := q.Limit
	if limit == 0 {
		limit = 10 // Default limit
	}

	rows, err := s.q.GetDocuments(ctx, GetDocumentsParams{
		KindPrefix:   q.KindPrefix,
		FiltersJsonb: filtersJSONB,
		LimitCount:   int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to execute document query: %w", err)
	}

	var documents []UserDocument
	for _, row := range rows {
		var doc UserDocument
		doc.ID = row.ExternalID
		doc.Source = row.Source
		doc.Kind = row.Kind
		doc.ContentType = row.ContentType
		doc.Body = row.Body
		doc.Timestamp = row.UpdatedAt.Time

		if err := json.Unmarshal(row.Attributes, &doc.Attributes); err != nil {
			return nil, fmt.Errorf("failed to unmarshal attributes: %w", err)
		}

		documents = append(documents, doc)
	}

	return documents, nil
}

// buildSearchResults converts a flat slice of rawSearchResult into the SearchResult slice.
// It groups rows by document, returns only the matched chunks from rawSearchResult for each document.
func (s *Store) buildSearchResults(raws []rawSearchResult) []SearchResult {
	// Group by DocumentID
	grouped := make(map[string][]rawSearchResult)
	for _, r := range raws {
		grouped[r.DocumentID] = append(grouped[r.DocumentID], r)
	}

	var resultsSlice []SearchResult
	for docID, docRows := range grouped {
		if len(docRows) == 0 {
			continue
		}

		// Preserve original chunk ordering
		sort.Slice(docRows, func(i, j int) bool {
			return docRows[i].ChunkIndex < docRows[j].ChunkIndex
		})

		first := docRows[0]

		// Collect only the matched chunks from rawSearchResult
		chunks := make([]string, len(docRows))
		for i, r := range docRows {
			chunks[i] = r.Chunk
		}

		// Compute best score for the document (higher is better)
		maxScore := docRows[0].Score
		for _, r := range docRows {
			if r.Score > maxScore {
				maxScore = r.Score
			}
		}

		sr := SearchResult{
			DocumentID: docID,
			Chunks:     chunks,
			Score:      maxScore,
			Timestamp:  first.Timestamp,
			Source:     first.Source,
			Kind:       first.Kind,
			Attributes: first.Attributes,
		}

		resultsSlice = append(resultsSlice, sr)
	}

	return resultsSlice
}
