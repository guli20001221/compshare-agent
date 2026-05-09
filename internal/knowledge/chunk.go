package knowledge

type KBChunk struct {
	ChunkID          string   `json:"chunk_id"`
	KBVersion        string   `json:"kb_version"`
	SourceType       string   `json:"source_type"`
	ProductArea      string   `json:"product_area"`
	ACL              string   `json:"acl"`
	ValidFrom        string   `json:"valid_from,omitempty"`
	ValidTo          *string  `json:"valid_to,omitempty"`
	Confidence       string   `json:"confidence"`
	Title            string   `json:"title"`
	QuestionPatterns []string `json:"question_patterns,omitempty"`
	Content          string   `json:"content"`
	SourceURL        string   `json:"source_url,omitempty"`
}

type Corpus struct {
	KBVersion string
	Chunks    []KBChunk
}
