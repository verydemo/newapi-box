package proxy

import (
	"sync"
	"time"
)

// eventLogCap bounds the in-memory conversion history. The log exists so an
// operator can watch traffic from the admin UI without reading server logs.
const eventLogCap = 200

// Event is one completed conversion, as shown in the admin UI.
type Event struct {
	Seq              uint64 `json:"seq"`
	Time             string `json:"time"`
	Client           string `json:"client"`
	Upstream         string `json:"upstream"`
	Model            string `json:"model"`
	UpstreamModel    string `json:"upstream_model,omitempty"`
	Stream           bool   `json:"stream"`
	Status           int    `json:"status"`
	LatencyMS        int64  `json:"latency_ms"`
	Converter        string `json:"converter,omitempty"`
	Diagnostics      int    `json:"diagnostics"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	Error            string `json:"error,omitempty"`
}

// eventLog is a fixed-size ring buffer with a monotonic sequence, so the UI
// can poll incrementally instead of re-reading the whole history.
type eventLog struct {
	mu    sync.Mutex
	seq   uint64
	items []Event
}

func newEventLog() *eventLog {
	return &eventLog{items: make([]Event, 0, eventLogCap)}
}

func (l *eventLog) append(event Event) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	event.Seq = l.seq
	if len(l.items) >= eventLogCap {
		// Drop the oldest entry. Copy preserves order without reallocating.
		copy(l.items, l.items[1:])
		l.items[len(l.items)-1] = event
		return
	}
	l.items = append(l.items, event)
}

// since returns every event newer than seq, oldest first. Passing 0 returns
// the whole retained history.
func (l *eventLog) since(seq uint64) []Event {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	out := make([]Event, 0, len(l.items))
	for _, event := range l.items {
		if event.Seq > seq {
			out = append(out, event)
		}
	}
	return out
}

// latestSeq lets the UI detect that it has fallen behind the retained window.
func (l *eventLog) latestSeq() uint64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

func nowStamp() string {
	return time.Now().Format(time.RFC3339)
}
