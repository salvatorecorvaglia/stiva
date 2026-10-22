package console

import "net/http"

func (h *Handler) getStorageUsage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.usage == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Storage summary unavailable"})
		return
	}
	snapshot := h.usage.Snapshot()
	status := http.StatusOK
	if snapshot.Usage == nil {
		status = http.StatusAccepted
		if snapshot.Error != "" {
			status = http.StatusServiceUnavailable
		}
	}
	writeJSON(w, status, snapshot)
}
