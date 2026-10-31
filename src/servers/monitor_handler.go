package servers

import (
	"net/http"
	"strconv"

	"github.com/bililive-go/bililive-go/src/monitor"
)

// getMonitorSamples 返回最近的人气采样与采样器状态。
func getMonitorSamples(w http.ResponseWriter, r *http.Request) {
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	mgr := monitor.Default()
	if mgr == nil {
		writeJSON(w, map[string]any{
			"samples": []monitor.Sample{},
			"status":  monitor.Status{Running: false},
		})
		return
	}

	writeJSON(w, map[string]any{
		"samples": mgr.Samples(limit),
		"status":  mgr.Snapshot(),
	})
}
