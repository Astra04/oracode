// mcp_server_logging.go
// Real working additions to MCPServer — cache logging wrappers.
// effectGraphCached and invalidateEffectGraph are called from tool handlers
// in mcp_server.go to add cache-hit/miss logging on top of idx.GetEffectGraph().

package oracode

// effectGraphCached returns the effect graph via the index's built-in double-checked
// cache (effectGraphMu + effectGraphDirty on Index), adding a cache-hit/miss log line.
func (s *MCPServer) effectGraphCached() (*EffectGraph, error) {
	graph, err := s.idx.GetEffectGraph()
	if err != nil {
		LogCacheMiss("effect_graph", err.Error())
		return nil, err
	}
	if graph == nil {
		LogCacheMiss("effect_graph", "not_built")
		return nil, nil
	}
	LogCacheHit("effect_graph")
	return graph, nil
}

// invalidateEffectGraph marks the index's effect graph dirty so the next call to
// effectGraphCached reloads from disk, and logs which event triggered the invalidation.
func (s *MCPServer) invalidateEffectGraph(trigger string) {
	s.idx.MarkEffectGraphDirty()
	LogCacheInvalidated("effect_graph", trigger)
}
