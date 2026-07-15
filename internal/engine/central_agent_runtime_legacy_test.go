package engine

// enableCentralAgentRuntimeForTest keeps pre-cutover unit fixtures explicit
// while the legacy semantic tests are migrated and deleted in P7c. Production
// code has no setter and therefore cannot switch a session back to that path.
func (e *Engine) enableCentralAgentRuntimeForTest() {
	e.centralAgentRuntimeEnabled = true
}
