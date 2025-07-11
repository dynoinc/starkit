// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.29.0

package ragstore

import (
	"github.com/jackc/pgx/v5/pgtype"
	pgvector_go "github.com/pgvector/pgvector-go"
)

type Chunk struct {
	ID               int32              `json:"id"`
	DocumentID       int32              `json:"document_id"`
	ChunkIndex       int32              `json:"chunk_index"`
	EmbeddingModelID pgtype.Int4        `json:"embedding_model_id"`
	ChunkText        string             `json:"chunk_text"`
	Embedding        pgvector_go.Vector `json:"embedding"`
}

type Document struct {
	ID          int32            `json:"id"`
	ExternalID  string           `json:"external_id"`
	Source      string           `json:"source"`
	Kind        string           `json:"kind"`
	ContentType string           `json:"content_type"`
	Attributes  []byte           `json:"attributes"`
	Body        string           `json:"body"`
	CreatedAt   pgtype.Timestamp `json:"created_at"`
	UpdatedAt   pgtype.Timestamp `json:"updated_at"`
}

type EmbeddingModel struct {
	ID         int32            `json:"id"`
	Name       string           `json:"name"`
	Dimensions int32            `json:"dimensions"`
	CreatedAt  pgtype.Timestamp `json:"created_at"`
}
