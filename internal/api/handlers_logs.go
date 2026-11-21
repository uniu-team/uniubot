package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/uniu-network/uniubot/internal/core/logging"
)

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	after := int64(queryInt(r, "after", 0))
	level := r.URL.Query().Get("level")
	search := r.URL.Query().Get("search")
	limit := clamp(queryInt(r, "limit", 500), 1, 2000)

	entries := s.logger.BufferAfter(after, level, search)
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"total":   len(entries),
	})
}

// handleLogsStream streams log entries as Server-Sent Events.
func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	entries := make(chan logging.Entry, 256)
	unsubscribe := s.logger.Subscribe(func(entry logging.Entry) {
		select {
		case entries <- entry:
		default:
		}
	})
	defer unsubscribe()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case entry := <-entries:
			payload, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
