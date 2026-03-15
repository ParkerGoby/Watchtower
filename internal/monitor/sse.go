package monitor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// sseClient represents one connected SSE subscriber.
type sseClient struct {
	ch   chan []byte
	done <-chan struct{} // closed when the HTTP request is cancelled
}

// SSEBroadcaster fans out SSE events to all connected clients.
type SSEBroadcaster struct {
	mu      sync.Mutex
	clients map[*sseClient]struct{}
}

// NewSSEBroadcaster creates an empty broadcaster.
func NewSSEBroadcaster() *SSEBroadcaster {
	return &SSEBroadcaster{clients: make(map[*sseClient]struct{})}
}

// ServeHTTP upgrades the connection to an SSE stream and blocks until the client disconnects.
func (b *SSEBroadcaster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	client := &sseClient{
		ch:   make(chan []byte, 8),
		done: r.Context().Done(),
	}
	b.mu.Lock()
	b.clients[client] = struct{}{}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.clients, client)
		b.mu.Unlock()
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-client.ch:
			fmt.Fprintf(w, "%s", msg)
			flusher.Flush()
		}
	}
}

// BroadcastState sends a "state" SSE event to all connected clients.
func (b *SSEBroadcaster) BroadcastState(snap StateSnapshot) {
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	msg := fmt.Sprintf("event: state\ndata: %s\n\n", data)
	b.broadcast([]byte(msg))
}

// BroadcastIncident sends an "incident" SSE event to all connected clients.
func (b *SSEBroadcaster) BroadcastIncident(inc Incident) {
	data, err := json.Marshal(inc)
	if err != nil {
		return
	}
	msg := fmt.Sprintf("event: incident\ndata: %s\n\n", data)
	b.broadcast([]byte(msg))
}

func (b *SSEBroadcaster) broadcast(msg []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for client := range b.clients {
		select {
		case client.ch <- msg:
		default:
			// Slow client — drop the message rather than blocking.
		}
	}
}

// ClientCount returns the number of connected clients (for testing/metrics).
func (b *SSEBroadcaster) ClientCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients)
}
