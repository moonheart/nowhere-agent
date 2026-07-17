package chatapi

import (
	"strings"
	"testing"
)

func TestStreamWriterEmitsValidFrames(t *testing.T) {
	w := newStreamWriter("msg1")
	w.start()
	w.textStart("t1")
	w.textDelta("t1", "Hello")
	w.textDelta("t1", " world")
	w.textEnd("t1")
	w.finish()
	w.done()

	out := w.String()

	mustContain := []string{
		`data: {"messageId":"msg1","type":"start"}`,
		`"type":"text-start"`,
		`"delta":"Hello"`,
		`"type":"text-end"`,
		`"type":"finish"`,
		"data: [DONE]",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestStreamWriterToolCall(t *testing.T) {
	w := newStreamWriter("m")
	w.toolCallStart("tc1", "read")
	w.toolCallDelta("tc1", `{"path":"/x"}`)
	w.toolCallEnd("tc1")
	w.toolResult("tc1", "file contents", false)
	out := w.String()
	for _, want := range []string{`"type":"tool-call-start"`, `"toolCallId":"tc1"`, `"type":"tool-result"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestStreamWriterError(t *testing.T) {
	w := newStreamWriter("m")
	w.error("something broke")
	if !strings.Contains(w.String(), `"errorText":"something broke"`) {
		t.Errorf("error chunk missing: %s", w.String())
	}
}
