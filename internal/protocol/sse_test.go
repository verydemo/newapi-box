package protocol

import (
	"strings"
	"testing"
)

func TestReaderParsesNamedAndBareFrames(t *testing.T) {
	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start"}`,
		"",
		": keep-alive",
		"",
		`data: {"n":1}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	reader := NewReader(strings.NewReader(stream))

	first, ok, err := reader.Next()
	if err != nil || !ok {
		t.Fatalf("first frame: ok=%v err=%v", ok, err)
	}
	if first.Name != "message_start" || first.Data != `{"type":"message_start"}` {
		t.Fatalf("first frame = %+v", first)
	}

	second, ok, err := reader.Next()
	if err != nil || !ok {
		t.Fatalf("second frame: ok=%v err=%v", ok, err)
	}
	if second.Name != "" || second.Data != `{"n":1}` {
		t.Fatalf("second frame = %+v", second)
	}

	third, ok, err := reader.Next()
	if err != nil || !ok {
		t.Fatalf("third frame: ok=%v err=%v", ok, err)
	}
	if third.Data != "[DONE]" {
		t.Fatalf("third frame = %+v", third)
	}

	if _, ok, _ := reader.Next(); ok {
		t.Fatal("expected end of stream")
	}
}

func TestReaderJoinsMultilineData(t *testing.T) {
	reader := NewReader(strings.NewReader("data: line1\ndata: line2\n\n"))

	event, ok, err := reader.Next()
	if err != nil || !ok {
		t.Fatalf("frame: ok=%v err=%v", ok, err)
	}
	if event.Data != "line1\nline2" {
		t.Fatalf("data = %q", event.Data)
	}
}

func TestReaderFlushesUnterminatedFrame(t *testing.T) {
	// Upstreams that close without a trailing blank line are common; the last
	// frame must not be dropped.
	reader := NewReader(strings.NewReader(`data: {"n":1}`))

	event, ok, err := reader.Next()
	if err != nil || !ok {
		t.Fatalf("frame: ok=%v err=%v", ok, err)
	}
	if event.Data != `{"n":1}` {
		t.Fatalf("data = %q", event.Data)
	}
}

func TestWriterFraming(t *testing.T) {
	var out strings.Builder
	writer := NewWriter(&out)

	if err := writer.WriteData(`{"a":1}`); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteEvent("message_stop", `{"type":"message_stop"}`); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteDone(); err != nil {
		t.Fatal(err)
	}

	want := "data: {\"a\":1}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n" +
		"data: [DONE]\n\n"
	if out.String() != want {
		t.Fatalf("got:\n%q\nwant:\n%q", out.String(), want)
	}
}
