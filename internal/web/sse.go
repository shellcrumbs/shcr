package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/shellcrumbs/shcr/internal/store"
)

// The web server and the daemon are separate processes, so there is no channel
// between them to listen on. The broker instead watches the event log's
// high-water mark — an indexed integer comparison, cheap enough to run every
// second — and pushes whatever changed. Neither process needs to know the other
// exists.
const pollInterval = time.Second

type broker struct {
	store  *store.Store
	logger *log.Logger

	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newBroker(st *store.Store, logger *log.Logger) *broker {
	return &broker{store: st, logger: logger, clients: map[chan []byte]struct{}{}}
}

func (b *broker) subscribe() (<-chan []byte, func()) {
	// Buffered so a slow reader cannot stall the poller; if it overflows the
	// client simply misses an update and the next one carries the state anyway.
	ch := make(chan []byte, 8)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if _, ok := b.clients[ch]; ok {
			delete(b.clients, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

func (b *broker) publish(payload []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- payload:
		default: // drop rather than block the poll loop
		}
	}
}

func (b *broker) hasClients() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.clients) > 0
}

func (b *broker) run(ctx context.Context) {
	last, err := b.store.MaxEventRowID()
	if err != nil {
		b.logger.Printf("event stream: %v", err)
	}

	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// With nobody watching there is nothing to do; the mark is re-read on
		// the next tick, so no updates are lost.
		if !b.hasClients() {
			if cur, err := b.store.MaxEventRowID(); err == nil {
				last = cur
			}
			continue
		}

		ids, next, err := b.store.EventsSince(last)
		if err != nil {
			b.logger.Printf("event stream: %v", err)
			continue
		}
		last = next
		if len(ids) == 0 {
			continue
		}

		changed := make([]store.Command, 0, len(ids))
		for _, id := range ids {
			c, err := b.store.CommandByID(id)
			if err != nil || c == nil {
				continue
			}
			changed = append(changed, *c)
		}
		if len(changed) == 0 {
			continue
		}
		payload, err := json.Marshal(map[string]any{"commands": changed})
		if err != nil {
			continue
		}
		b.publish(payload)
	}
}

// handleEvents streams row changes so the dashboard updates without polling.
// SSE rather than WebSockets: the traffic is one-directional, it is a few lines
// of code on both sides, and the browser reconnects on its own.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Without this, a proxy in front of the dashboard would buffer the stream
	// into uselessness.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ch, unsubscribe := s.broker.subscribe()
	defer unsubscribe()

	fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()

	// A periodic comment keeps intermediaries from timing out an idle stream.
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case payload, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "event: commands\ndata: %s\n\n", payload)
			flusher.Flush()
		}
	}
}
