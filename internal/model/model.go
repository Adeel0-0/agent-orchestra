// Package model holds the event, handoff and state types shared with the
// Python implementation (orchestra/models.py). The JSON field names and the
// string values are an on-disk format: both implementations read and write them.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// EventType values are written to events.jsonl verbatim.
type EventType string

const (
	AgentClaim          EventType = "AGENT_CLAIM"
	AgentHeartbeat      EventType = "AGENT_HEARTBEAT"
	TaskProgress        EventType = "TASK_PROGRESS"
	HandoffRequest      EventType = "HANDOFF_REQUEST"
	ReviewRequest       EventType = "REVIEW_REQUEST"
	ReviewVerdict       EventType = "REVIEW_VERDICT"
	VerificationStarted EventType = "VERIFICATION_STARTED"
	VerificationResult  EventType = "VERIFICATION_RESULT"
	GitCommitted        EventType = "GIT_COMMITTED"
	GitSynced           EventType = "GIT_SYNCED"
	GitSyncFailed       EventType = "GIT_SYNC_FAILED"
	CIUpdate            EventType = "CI_UPDATE"
	EmergencyStop       EventType = "EMERGENCY_STOP"
	ResumeWork          EventType = "RESUME_WORK"
	CustomEvent         EventType = "CUSTOM_EVENT"
)

var knownEventTypes = map[EventType]bool{
	AgentClaim: true, AgentHeartbeat: true, TaskProgress: true, HandoffRequest: true,
	ReviewRequest: true, ReviewVerdict: true, VerificationStarted: true, VerificationResult: true,
	GitCommitted: true, GitSynced: true, GitSyncFailed: true, CIUpdate: true,
	EmergencyStop: true, ResumeWork: true, CustomEvent: true,
}

// ParseEventType returns the matching type, or CUSTOM_EVENT for anything unrecognized.
func ParseEventType(s string) EventType {
	if t := EventType(s); knownEventTypes[t] {
		return t
	}
	return CustomEvent
}

// State values match OrchestraState in Python.
type State string

const (
	IdleClean        State = "IDLE_CLEAN"
	WriterActive     State = "WRITER_ACTIVE"
	ReadyNeedsVerify State = "READY_NEEDS_VERIFY"
	Verifying        State = "VERIFYING"
	ReadyVerified    State = "READY_VERIFIED"
	Syncing          State = "SYNCING"
	CIMonitoring     State = "CI_MONITORING"
	PausedCheckpoint State = "PAUSED_CHECKPOINT"
	Error            State = "ERROR"
)

// AgentEvent is one line of events.jsonl. Field order matches Python's to_dict().
type AgentEvent struct {
	EventID     string         `json:"event_id"`
	EventType   EventType      `json:"event_type"`
	SourceAgent string         `json:"source_agent"`
	Payload     map[string]any `json:"payload"`
	Timestamp   float64        `json:"timestamp"`
}

// NewEventID returns "evt_" plus 12 lowercase hex characters, like Python's uuid4().hex[:12].
func NewEventID() string {
	return "evt_" + RandomHex(6)
}

// RandomHex returns 2*n lowercase hex characters.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// Now returns the current time as float Unix seconds, like Python's time.time().
func Now() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

// ParseEvent decodes one events.jsonl line the way AgentEvent.from_dict does:
// a missing event_id is "", a missing source_agent is "unknown", an unknown
// event type becomes CUSTOM_EVENT, a missing timestamp means now, and a
// payload that is not an object is an error (Python's dict() raises, and the
// line is skipped).
func ParseEvent(line []byte) (AgentEvent, error) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return AgentEvent{}, err
	}
	if raw == nil {
		return AgentEvent{}, fmt.Errorf("event is not a JSON object")
	}
	evt := AgentEvent{
		EventID:     PyStr(raw, "event_id", ""),
		EventType:   ParseEventType(PyStr(raw, "event_type", string(CustomEvent))),
		SourceAgent: PyStr(raw, "source_agent", "unknown"),
		Payload:     map[string]any{},
		Timestamp:   Now(),
	}
	if p, ok := raw["payload"]; ok {
		obj, isObj := p.(map[string]any)
		if !isObj {
			return AgentEvent{}, fmt.Errorf("payload is not an object")
		}
		evt.Payload = obj
	}
	if ts, ok := raw["timestamp"]; ok {
		f, err := PyFloat(ts)
		if err != nil {
			return AgentEvent{}, err
		}
		evt.Timestamp = f
	}
	return evt, nil
}

// HandoffPayload mirrors orchestra.models.HandoffPayload.
type HandoffPayload struct {
	TaskID       string
	AgentID      string
	Status       string // "complete" | "paused" | "failed" | "unknown"
	Summary      string
	ChangedFiles []any
	TestResults  map[string]any
	Timestamp    float64
}

// HandoffFromDict mirrors HandoffPayload.from_dict. A missing status is
// "unknown": a handoff that doesn't say it's complete is never treated as complete.
func HandoffFromDict(data map[string]any) HandoffPayload {
	h := HandoffPayload{
		TaskID:      PyStr(data, "task_id", ""),
		AgentID:     PyStr(data, "agent_id", ""),
		Status:      PyStr(data, "status", "unknown"),
		Summary:     PyStr(data, "summary", ""),
		TestResults: map[string]any{},
		Timestamp:   Now(),
	}
	if files, ok := data["changed_files"].([]any); ok {
		h.ChangedFiles = files
	}
	if results, ok := data["test_results"].(map[string]any); ok {
		h.TestResults = results
	}
	if ts, ok := data["timestamp"]; ok {
		if f, err := PyFloat(ts); err == nil {
			h.Timestamp = f
		}
	}
	return h
}

// PyStr returns str(data.get(key, def)) the way Python formats common JSON values.
func PyStr(data map[string]any, key, def string) string {
	v, ok := data[key]
	if !ok {
		return def
	}
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e16 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// PyFloat converts a JSON number (or numeric string) like Python's float().
func PyFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case string:
		return strconv.ParseFloat(x, 64)
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("not a number: %v", v)
	}
}

// LockExpiresAt returns lock["expires_at"] as a float, treating a missing or
// non-numeric value as 0, like current.get("expires_at", 0) in Python.
func LockExpiresAt(lock map[string]any) float64 {
	if lock == nil {
		return 0
	}
	f, err := PyFloat(lock["expires_at"])
	if err != nil {
		return 0
	}
	return f
}
