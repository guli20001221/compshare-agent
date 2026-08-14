package knowledge

// Schema notes:
//   - SourceOrigin records whether the chunk came from official docs or a
//     customer-safe curated FAQ. New chunks must set it explicitly.
//   - SurfaceURL is the public URL intended for a future user-facing citation
//     path. Current production chunks emit this JSON field as null; Go now
//     recognizes it but rendering still uses the legacy SourceURL branch.
//   - SourceURL is the legacy citation field. The deployed curated corpus still
//     uses it, so readers retain it until that corpus has migrated to SurfaceURL;
//     new chunks should use SurfaceURL instead.
type KBChunk struct {
	ChunkID          string   `json:"chunk_id"`
	KBVersion        string   `json:"kb_version"`
	SourceType       string   `json:"source_type"`
	SourceOrigin     string   `json:"source_origin"`
	ProductArea      string   `json:"product_area"`
	ACL              string   `json:"acl"`
	ValidFrom        string   `json:"valid_from,omitempty"`
	ValidTo          *string  `json:"valid_to,omitempty"`
	Confidence       string   `json:"confidence"`
	Title            string   `json:"title"`
	QuestionPatterns []string `json:"question_patterns,omitempty"`
	Content          string   `json:"content"`
	// ContentTruncated is set only for a body returned by the remote MCP Read
	// operation. It tells the caller that compshare-kb applied its own response
	// limit before the agent's local context limit runs. It is intentionally not
	// persisted with the corpus: source chunks are always the canonical bodies.
	ContentTruncated bool `json:"-"`
	// SourceURL is retained only for existing curated-corpus citations. New
	// chunks should use SurfaceURL for user-facing public URLs.
	SourceURL  string  `json:"source_url,omitempty"`
	SurfaceURL *string `json:"surface_url,omitempty"`

	// V2 provenance and hierarchy fields are optional so the loader remains
	// backward-compatible with the legacy W0 corpus. V2 preprocessing has
	// emitted the first group since its initial release; retaining them here
	// prevents json.Unmarshal from silently discarding information the runtime
	// needs for precise retrieval and incremental source updates.
	DocumentID    string   `json:"document_id,omitempty"`
	DocumentTitle string   `json:"document_title,omitempty"`
	DocumentType  string   `json:"document_type,omitempty"`
	HeadingPath   []string `json:"heading_path,omitempty"`
	ChunkRole     string   `json:"chunk_role,omitempty"`
	EvidenceKind  string   `json:"evidence_kind,omitempty"`
	SourceRefs    []string `json:"source_refs,omitempty"`
	V2SourceKind  string   `json:"v2_source_kind,omitempty"`

	// The fields below are emitted by V2-native incremental updates. ParentID
	// is the stable document-level parent for this child chunk; ChunkOrdinal
	// gives its position within that parent. ExactTerms is a bounded list of
	// curated / extracted identifiers (models, error codes, API names, etc.)
	// used only as an additional RRF candidate leg, never as a hard exclusion.
	SourceRevision string   `json:"source_revision,omitempty"`
	ParentID       string   `json:"parent_id,omitempty"`
	ChunkOrdinal   int      `json:"chunk_ordinal,omitempty"`
	ExactTerms     []string `json:"exact_terms,omitempty"`
}

type Corpus struct {
	KBVersion string
	Chunks    []KBChunk
}
