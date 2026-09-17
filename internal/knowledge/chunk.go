package knowledge

// MaxKnowledgeContentRunes is the longest chunk body the knowledge service
// serves; the engine and the SSH-ops bridge size their read budgets from it.
const MaxKnowledgeContentRunes = 4000

// KBChunk is one knowledge chunk as the remote knowledge service returns it.
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
	// limit before the agent's local context limit runs.
	ContentTruncated bool `json:"-"`
	// SourceURL is retained only for existing curated-corpus citations. New
	// chunks should use SurfaceURL for user-facing public URLs.
	SourceURL  string  `json:"source_url,omitempty"`
	SurfaceURL *string `json:"surface_url,omitempty"`
}
