package agentpool

// SizeForTest returns the current number of cached engines.
// It must not be used outside of _test files.
func (p *Pool) SizeForTest() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.items)
}
