// Package protocol maps HTTP surfaces and wire events onto relaykit DTOs.
//
// relaykit deliberately stops at the DTO boundary: it neither parses nor
// emits SSE. Everything below the DTO — framing, event names, the
// `data: [DONE]` sentinel — is protocol plumbing and therefore lives here.
package protocol

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// maxEventBytes bounds a single SSE event. Upstreams are known to emit very
// large frames (image payloads, long tool arguments), so this is generous.
const maxEventBytes = 4 << 20

// Event is one decoded SSE frame.
type Event struct {
	Name string
	Data string
}

// Reader decodes an SSE byte stream into frames.
type Reader struct {
	scanner *bufio.Scanner
}

func NewReader(r io.Reader) *Reader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
	return &Reader{scanner: scanner}
}

// Next returns the next frame. ok is false at end of stream.
//
// Per the SSE spec a frame ends at a blank line; `data:` lines accumulate and
// are joined with "\n". Comment lines (": ping") are skipped because relaykit
// has no representation for them.
func (r *Reader) Next() (Event, bool, error) {
	var (
		event  Event
		datas  []string
		sawAny bool
	)

	for r.scanner.Scan() {
		line := strings.TrimRight(r.scanner.Text(), "\r")
		if line == "" {
			if !sawAny {
				continue
			}
			event.Data = strings.Join(datas, "\n")
			return event, true, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			// A bare field name is valid SSE and means "empty value".
			sawAny = true
			continue
		}
		value = strings.TrimPrefix(value, " ")

		switch field {
		case "event":
			event.Name = value
			sawAny = true
		case "data":
			datas = append(datas, value)
			sawAny = true
		default:
			// id / retry are not needed for relay conversion.
			sawAny = true
		}
	}

	if err := r.scanner.Err(); err != nil {
		return Event{}, false, fmt.Errorf("read sse stream: %w", err)
	}
	// Flush a trailing frame that was not terminated by a blank line.
	if sawAny {
		event.Data = strings.Join(datas, "\n")
		return event, true, nil
	}
	return Event{}, false, nil
}

// Writer emits SSE frames.
type Writer struct {
	w io.Writer
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// WriteData emits a data-only frame, used by the OpenAI Chat and Gemini
// surfaces.
func (w *Writer) WriteData(data string) error {
	_, err := io.WriteString(w.w, "data: "+data+"\n\n")
	return err
}

// WriteEvent emits a named frame, used by the Claude Messages and OpenAI
// Responses surfaces.
func (w *Writer) WriteEvent(name string, data string) error {
	if _, err := io.WriteString(w.w, "event: "+name+"\n"); err != nil {
		return err
	}
	_, err := io.WriteString(w.w, "data: "+data+"\n\n")
	return err
}

// WriteDone emits the OpenAI Chat end-of-stream sentinel.
func (w *Writer) WriteDone() error {
	return w.WriteData("[DONE]")
}

// WriteComment emits an SSE comment, used as a keep-alive ping.
func (w *Writer) WriteComment(text string) error {
	_, err := io.WriteString(w.w, ": "+text+"\n\n")
	return err
}
