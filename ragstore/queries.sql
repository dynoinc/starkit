-- Document operations
-- name: UpsertDocument :one
INSERT INTO documents (external_id, source, kind, content_type, attributes, body, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (external_id) DO UPDATE
SET
    source = EXCLUDED.source,
    kind = EXCLUDED.kind,
    content_type = EXCLUDED.content_type,
    attributes = EXCLUDED.attributes,
    body = EXCLUDED.body,
    updated_at = EXCLUDED.updated_at
RETURNING id;

-- name: GetDocument :one
SELECT external_id, source, kind, content_type, attributes, body, updated_at 
FROM documents 
WHERE external_id = $1;

-- name: DeleteDocument :exec
DELETE FROM documents 
WHERE external_id = $1;

-- name: DeleteDocumentsByKindPrefix :execrows
DELETE FROM documents
WHERE kind LIKE $1::text || '%';

-- name: GetDocumentIDByExternalID :one
SELECT id 
FROM documents 
WHERE external_id = $1;

-- name: GetDocuments :many
SELECT external_id, source, kind, content_type, attributes, body, updated_at
FROM documents
WHERE (@kind_prefix::text IS NULL OR kind LIKE @kind_prefix::text || '%')
    AND (@filters_jsonb::jsonb IS NULL OR attributes @> @filters_jsonb::jsonb)
ORDER BY updated_at DESC
LIMIT @limit_count::int;

-- Embedding model operations
-- name: GetEmbeddingModel :one
SELECT id, name, dimensions 
FROM embedding_models 
WHERE name = $1;

-- name: CreateEmbeddingModel :one
INSERT INTO embedding_models (name, dimensions) 
VALUES ($1, $2) 
ON CONFLICT (name) DO UPDATE
SET dimensions = EXCLUDED.dimensions
RETURNING id;

-- Chunk operations
-- name: DeleteChunksByDocumentID :exec
DELETE FROM chunks 
WHERE document_id = $1;

-- name: InsertChunk :exec
INSERT INTO chunks (document_id, chunk_index, chunk_text) 
VALUES ($1, $2, $3);

-- name: InsertChunkWithEmbedding :exec
INSERT INTO chunks (document_id, chunk_index, embedding_model_id, chunk_text, embedding) 
VALUES ($1, $2, $3, $4, $5);

-- name: GetAllChunksForDocID :many
SELECT chunk_text 
FROM chunks 
WHERE document_id = $1 
ORDER BY chunk_index ASC;

-- Search operations
-- name: LexicalSearch :many
SELECT
    d.external_id,
    c.chunk_text,
    c.chunk_index,
    ts_rank(to_tsvector('english', c.chunk_text), plainto_tsquery('english', @keyword_query)) as score,
    d.updated_at,
    d.source,
    d.kind,
    d.attributes
FROM chunks c
JOIN documents d ON c.document_id = d.id
WHERE to_tsvector('english', c.chunk_text) @@ plainto_tsquery('english', @keyword_query)
    AND (@kind_prefix::text IS NULL OR d.kind LIKE @kind_prefix::text || '%')
    AND (@filters_jsonb::jsonb IS NULL OR d.attributes @> @filters_jsonb::jsonb)
ORDER BY
    ts_rank(to_tsvector('english', c.chunk_text), plainto_tsquery('english', @keyword_query)) DESC
LIMIT @limit_count::int;

-- name: SemanticSearch :many
SELECT
    d.external_id,
    c.chunk_text,
    c.chunk_index,
    c.embedding <-> @embedding as score,
    d.updated_at,
    d.source,
    d.kind,
    d.attributes
FROM chunks c
JOIN documents d ON c.document_id = d.id
WHERE c.embedding IS NOT NULL
    AND (@kind_prefix::text IS NULL OR d.kind LIKE @kind_prefix::text || '%')
    AND (@filters_jsonb::jsonb IS NULL OR d.attributes @> @filters_jsonb::jsonb)
ORDER BY (c.embedding <-> @embedding) DESC
LIMIT @limit_count::int;