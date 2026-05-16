package knowledge

// Schema notes:
//   - SourceOrigin records whether the chunk came from official docs or a
//     customer-safe curated FAQ. New chunks must set it explicitly.
//   - SurfaceURL is the public URL intended for a future user-facing citation
//     path. Current production chunks emit this JSON field as null; Go now
//     recognizes it but rendering still uses the legacy SourceURL branch.
//   - SourceURL is legacy. Production W0 chunks do not carry it, and new chunks
//     should leave it empty. A later renderer PR can switch user-facing links to
//     SurfaceURL.
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
	// Deprecated: SourceURL is legacy and must be empty in new chunks.
	// Use SurfaceURL for user-facing public URLs.
	SourceURL  string  `json:"source_url,omitempty"`
	SurfaceURL *string `json:"surface_url,omitempty"`
}

type Corpus struct {
	KBVersion string
	Chunks    []KBChunk
}
