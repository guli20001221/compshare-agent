package agentpool

import (
	"github.com/compshare-agent/internal/engine"
	"github.com/compshare-agent/internal/store"
)

// FilterHistoryForTest exposes filterHistory for white-box unit testing.
// It must not be used outside of _test files.
func FilterHistoryForTest(messages []store.Message) []engine.HistoryMessage {
	return filterHistory(messages)
}

// SizeForTest returns the current number of cached engines.
// It must not be used outside of _test files.
func (p *Pool) SizeForTest() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}
