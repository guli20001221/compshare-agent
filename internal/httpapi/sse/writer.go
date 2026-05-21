package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// Writer serializes SSE events to an http.ResponseWriter.
// All methods are safe for concurrent use.
type Writer struct {
	w  http.ResponseWriter
	fl http.Flusher
	mu sync.Mutex
}

// New creates an SSE Writer. If w implements http.Flusher, each write will flush.
func New(w http.ResponseWriter) *Writer {
	fl, _ := w.(http.Flusher)
	return &Writer{w: w, fl: fl}
}

// WriteEvent marshals data to JSON and writes an SSE event frame, then flushes.
func (s *Writer) WriteEvent(event string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}

// WriteKeepalive writes an SSE comment keepalive frame, then flushes.
func (s *Writer) WriteKeepalive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprint(s.w, ":keepalive\n\n"); err != nil {
		return err
	}
	if s.fl != nil {
		s.fl.Flush()
	}
	return nil
}
