package runtime

import "expvar"

// SSE + heartbeat observability counters for Phase B go/no-go decisions.
//
// Three metrics feed the Phase B launch gate (v6 plan §4 E7):
//   - sse_active_conns:      gauge,  current live SSE connections
//   - sse_reconnect_total:   counter, cumulative SSE register calls
//   - heartbeat_fallback_hit_total: counter, heartbeat claims that SSE should have delivered
//
// Phase B thresholds: conn p50 > 30min, reconnect rate < 5%, heartbeat
// fallback hit rate < 1%. Without these metrics the decision is guesswork.
//
// expvar publishes via /debug/vars on the default mux; Prometheus scrapes
// with expvar_exporter or a custom collector. No extra dependency needed.
var (
	sseActiveConns           = expvar.NewInt("sse_active_conns")
	sseReconnectTotal        = expvar.NewInt("sse_reconnect_total")
	heartbeatFallbackHitTotal = expvar.NewInt("heartbeat_fallback_hit_total")
)
