package bus

import (
	"os"
	"strings"
	"testing"

	"github.com/Adeel0-0/agent-orchestra/internal/model"
)

func TestEmitAndReadEvents(t *testing.T) {
	b, _ := NewEventBus(t.TempDir())
	evt, err := b.Emit("AGENT_CLAIM", "claude", map[string]any{"task_id": "T1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(evt.EventID, "evt_") || len(evt.EventID) != 16 || strings.ToLower(evt.EventID) != evt.EventID {
		t.Fatalf("bad event id %q", evt.EventID)
	}
	b.Emit("NOT_A_REAL_TYPE", "codex", nil)

	events := b.ReadEvents(0, 100)
	if len(events) != 2 || events[0].Payload["task_id"] != "T1" || events[1].EventType != model.CustomEvent {
		t.Fatalf("unexpected events: %+v", events)
	}
	if latest := b.Latest(); latest == nil || latest.SourceAgent != "codex" {
		t.Fatalf("latest event: %+v", latest)
	}
	data, _ := os.ReadFile(b.EventsFile)
	if !strings.HasSuffix(string(data), "}\n") || strings.Contains(string(data), "\r") {
		t.Fatalf("events must be \\n-terminated lines: %q", data)
	}
}

func TestReadFromLeavesHalfWrittenLineForNextRead(t *testing.T) {
	b, _ := NewEventBus(t.TempDir())
	b.Emit("TASK_PROGRESS", "claude", map[string]any{"n": 1.0})
	events, offset, err := b.ReadFrom(0)
	if err != nil || len(events) != 1 {
		t.Fatalf("first read: %v %v", events, err)
	}

	f, _ := os.OpenFile(b.EventsFile, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"event_id": "evt_000000000002", "event_type": "TASK_PROGRESS", "source_agent": "codex", "payload": {"n": 2}, "timestamp": 1.0`)
	f.Close()
	if events, next, _ := b.ReadFrom(offset); len(events) != 0 || next != offset {
		t.Fatalf("half-written line was consumed: %v next=%d offset=%d", events, next, offset)
	}

	f, _ = os.OpenFile(b.EventsFile, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("}\n")
	f.Close()
	events, _, _ = b.ReadFrom(offset)
	if len(events) != 1 || events[0].Payload["n"] != 2.0 {
		t.Fatalf("completed line not read: %+v", events)
	}
}

func TestReadFromAcceptsCRLFAndRestartsAfterTruncation(t *testing.T) {
	b, _ := NewEventBus(t.TempDir())
	// What Python writes on Windows (text mode turns \n into \r\n).
	crlf := `{"event_id": "evt_aaaaaaaaaaaa", "event_type": "HANDOFF_REQUEST", "source_agent": "claude", "payload": {}, "timestamp": 5.5}` + "\r\n"
	os.WriteFile(b.EventsFile, []byte(crlf+crlf), 0o644)
	events, offset, _ := b.ReadFrom(0)
	if len(events) != 2 || events[0].EventType != model.HandoffRequest || events[0].Timestamp != 5.5 {
		t.Fatalf("CRLF lines not parsed: %+v", events)
	}
	os.WriteFile(b.EventsFile, []byte(crlf), 0o644) // rotated: smaller than our offset
	if events, _, _ := b.ReadFrom(offset); len(events) != 1 {
		t.Fatalf("did not restart after truncation: %+v", events)
	}
}

func TestBadLinesAreSkipped(t *testing.T) {
	b, _ := NewEventBus(t.TempDir())
	os.WriteFile(b.EventsFile, []byte("garbage\n{\"payload\": 3}\n{\"event_type\": \"GIT_SYNCED\"}\n"), 0o644)
	events, _, _ := b.ReadFrom(0)
	if len(events) != 1 || events[0].EventType != model.GitSynced || events[0].SourceAgent != "unknown" {
		t.Fatalf("got %+v", events)
	}
}
