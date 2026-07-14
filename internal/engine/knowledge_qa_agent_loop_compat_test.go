package engine

// Legacy test helpers keep older table setup readable while the production
// runtime has no knowledge-route toggle. They intentionally do nothing.
func SetKnowledgeQAAgentLoopEnabled(bool) {}

func KnowledgeQAAgentLoopEnabled() bool { return true }
