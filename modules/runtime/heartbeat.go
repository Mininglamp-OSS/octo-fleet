package runtime

const (
	minHeartbeatIntervalMs = int64(1000)
	maxHeartbeatIntervalMs = int64(300000)
)

// clampHeartbeatIntervalMs 把 daemon 上报的心跳间隔夹到 [1000,300000]ms;
// 0 表示"用 sweeper 默认",越界值回落 0(=默认),不报错。
func clampHeartbeatIntervalMs(ms int64) int64 {
	if ms == 0 {
		return 0
	}
	if ms < minHeartbeatIntervalMs || ms > maxHeartbeatIntervalMs {
		return 0
	}
	return ms
}
