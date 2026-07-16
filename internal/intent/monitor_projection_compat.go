package intent

import (
	"github.com/compshare-agent/internal/readprojection"
)

// The monitor time-window interpreter now has a single implementation in
// internal/readprojection. These forwarders keep the legacy HandleMonitorQuery
// handler compiling with the original bare names; new read capabilities call
// readprojection directly.

func isCurrentMonitorTimeWindow(w *TimeWindow) bool {
	return readprojection.IsCurrentMonitorTimeWindow(w)
}

func resolveMonitorHistoryWindow(w *TimeWindow) (int64, int64, bool) {
	return readprojection.ResolveMonitorHistoryWindow(w)
}
