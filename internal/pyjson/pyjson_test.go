package pyjson

import "testing"

type event struct {
	ID      string         `json:"event_id"`
	Payload map[string]any `json:"payload"`
	TS      float64        `json:"timestamp"`
}

func TestMarshalMatchesPythonDumps(t *testing.T) {
	got, err := Marshal(event{ID: "evt_1", Payload: map[string]any{"a": "x, y: z", "list": []any{1, "<é>"}}, TS: 1795262895.25})
	if err != nil {
		t.Fatal(err)
	}
	// json.dumps({"event_id": "evt_1", "payload": {"a": "x, y: z", "list": [1, "<é>"]}, "timestamp": 1795262895.25})
	want := `{"event_id": "evt_1", "payload": {"a": "x, y: z", "list": [1, "<\u00e9>"]}, "timestamp": 1795262895.25}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestEscapedQuotesInsideStringsAreLeftAlone(t *testing.T) {
	got, _ := Marshal(map[string]string{"k": `say "a,b": \ ok`})
	want := `{"k": "say \"a,b\": \\ ok"}`
	if string(got) != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestAstralCharacterBecomesSurrogatePair(t *testing.T) {
	got, _ := Marshal("🎻")
	if string(got) != `"\ud83c\udfbb"` {
		t.Fatalf("got %s", got)
	}
}

func TestMarshalIndentKeepsColonSpaceAndEscapes(t *testing.T) {
	got, _ := MarshalIndent(map[string]any{"agent_id": "ü", "pid": 7})
	want := "{\n  \"agent_id\": \"\\u00fc\",\n  \"pid\": 7\n}"
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
