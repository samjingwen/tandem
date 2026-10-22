// Package readmodel defines the additive, bounded v1 read protocol.
// A projection is not a replacement for the retained record: Content identifies
// the complete JSON record, including every omitted field and evidence entry.
package readmodel

const (
	MaxPageBytes = 256 << 10
	ContentBytes = 32 << 10
	MaxRecords   = 64
)

type Revision struct {
	InstanceID string `json:"instanceId"`
	Revision   int64  `json:"revision"`
}

type Manifest struct {
	Revision
	Collections  map[string]int `json:"collections"`
	MaxPageBytes int            `json:"maxPageBytes"`
	ContentBytes int            `json:"contentBytes"`
	MaxRecords   int            `json:"maxRecords"`
}

type ContentRef struct {
	Cursor string `json:"cursor"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type Record struct {
	Key     string         `json:"key"`
	Summary map[string]any `json:"summary"`
	Content ContentRef     `json:"content"`
}

type Page struct {
	Revision
	Collection string   `json:"collection"`
	Records    []Record `json:"records"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

type ContentPage struct {
	Revision
	Collection string `json:"collection"`
	Key        string `json:"key"`
	Offset     int    `json:"offset"`
	TotalBytes int    `json:"totalBytes"`
	Text       string `json:"text"`
	SHA256     string `json:"sha256"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// Receipt is negotiated with Prefer: return=bounded only when the ordinary
// mutation result exceeds the page budget. It refers to the exact durable
// operation outcome, never a later task snapshot or a speculative mutation.
type Receipt struct {
	Revision
	Representation string `json:"representation"`
	OperationID    string `json:"operationId"`
	Record         Record `json:"record"`
}
