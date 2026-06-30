package agentic

// QueryPlan is the structured plan for an agentic retrieval turn.
// RefIDs are scoped to a single response and are not durable document IDs.
type QueryPlan struct {
	Subqueries []Subquery `json:"subqueries"`
}

type Subquery struct {
	ID              string `json:"id"`
	Query           string `json:"query"`
	Purpose         string `json:"purpose,omitempty"`
	ProductAreaHint string `json:"product_area_hint,omitempty"`
	Required        bool   `json:"required"`
}

type RetrievalActivity struct {
	ID                     string   `json:"id"`
	SubqueryID             string   `json:"subquery_id,omitempty"`
	Query                  string   `json:"query"`
	Purpose                string   `json:"purpose,omitempty"`
	ProductAreaHint        string   `json:"product_area_hint,omitempty"`
	Required               bool     `json:"required,omitempty"`
	LatencyMS              int64    `json:"latency_ms,omitempty"`
	Hits                   int      `json:"hits"`
	KeptHits               int      `json:"kept_hits,omitempty"`
	FloorDroppedAll        bool     `json:"floor_dropped_all,omitempty"`
	HybridMode             string   `json:"hybrid_mode,omitempty"`
	HybridFallbackReason   string   `json:"hybrid_fallback_reason,omitempty"`
	RerankerMode           string   `json:"reranker_mode,omitempty"`
	RerankerFallbackReason string   `json:"reranker_fallback_reason,omitempty"`
	Error                  string   `json:"error,omitempty"`
	RetrievedChunkIDs      []string `json:"retrieved_chunk_ids,omitempty"`
	KeptChunkIDs           []string `json:"kept_chunk_ids,omitempty"`
}

type Reference struct {
	RefID       string   `json:"ref_id"`
	ChunkID     string   `json:"chunk_id"`
	Title       string   `json:"title,omitempty"`
	SourceURL   string   `json:"source_url,omitempty"`
	Score       float64  `json:"score,omitempty"`
	SourceArea  string   `json:"source_area,omitempty"`
	ActivityIDs []string `json:"activity_ids,omitempty"`
	Rank        int      `json:"rank,omitempty"`
}

type ReferenceLedger struct {
	RefIDScheme string      `json:"ref_id_scheme,omitempty"`
	References  []Reference `json:"references"`
}

type AgenticRetrievalResult struct {
	QueryPlan                    QueryPlan           `json:"query_plan"`
	Activities                   []RetrievalActivity `json:"activities"`
	ReferenceLedger              ReferenceLedger     `json:"reference_ledger"`
	RetrievedChunkIDs            []string            `json:"retrieved_chunk_ids,omitempty"`
	FusionRerankerFallbackReason string              `json:"fusion_reranker_fallback_reason,omitempty"`
	FusionRerankerLatencyMS      int64               `json:"fusion_reranker_latency_ms,omitempty"`
}
