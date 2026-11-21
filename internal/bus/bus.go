// Package bus is the file-based event bus (events.jsonl) and the writer lock,
// sharing the on-disk format with orchestra/bus.py so both can run at once.
package bus

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Adeel0-0/agent-orchestra/internal/model"
	"github.com/Adeel0-0/agent-orchestra/internal/pyjson"
)

// EventBus appends events to <dir>/events.jsonl and mirrors the latest to latest_event.json.
type EventBus struct {
	Dir         string
	EventsFile  string
	LatestEvent string
}

// NewEventBus creates the directory if needed.
func NewEventBus(dir string) (*EventBus, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &EventBus{
		Dir:         abs,
		EventsFile:  filepath.Join(abs, "events.jsonl"),
		LatestEvent: filepath.Join(abs, "latest_event.json"),
	}, nil
}

// Emit appends one event. An unrecognized type becomes CUSTOM_EVENT.
// The line is written with a single append so concurrent writers never interleave.
func (b *EventBus) Emit(eventType string, source string, payload map[string]any) (model.AgentEvent, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	evt := model.AgentEvent{
		EventID:     model.NewEventID(),
		EventType:   model.ParseEventType(eventType),
		SourceAgent: source,
		Payload:     payload,
		Timestamp:   model.Now(),
	}
	encoded, err := pyjson.Marshal(evt)
	if err != nil {
		return evt, err
	}
	line := append(encoded, '\n')

	f, err := os.OpenFile(b.EventsFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return evt, err
	}
	_, writeErr := f.Write(line)
	closeErr := f.Close()
	if writeErr != nil {
		return evt, writeErr
	}
	if closeErr != nil {
		return evt, closeErr
	}

	// Best effort, like Python: the log above is the source of truth.
	tmp := filepath.Join(b.Dir, ".latest_"+model.RandomHex(3)+".tmp")
	if err := os.WriteFile(tmp, line, 0o644); err == nil {
		if err := retrySharing(func() error { return os.Rename(tmp, b.LatestEvent) }); err != nil {
			os.Remove(tmp)
		}
	} else {
		os.Remove(tmp)
	}
	return evt, nil
}

// EndOffset is the size of events.jsonl, or 0 if it does not exist.
func (b *EventBus) EndOffset() int64 {
	info, err := os.Stat(b.EventsFile)
	if err != nil {
		return 0
	}
	return info.Size()
}

// ReadFrom returns the complete events after byte offset and the offset to resume from.
// A trailing line with no newline (a writer is mid-write) is left for the next call.
// If the file shrank, reading restarts from 0. Both "\n" and "\r\n" endings are accepted.
func (b *EventBus) ReadFrom(offset int64) ([]model.AgentEvent, int64, error) {
	size := b.EndOffset()
	if size < offset {
		offset = 0
	}
	if size == offset {
		return nil, offset, nil
	}
	f, err := os.Open(b.EventsFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, offset, err
	}
	defer f.Close()
	chunk := make([]byte, size-offset)
	n, err := f.ReadAt(chunk, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, offset, err
	}
	chunk = chunk[:n]
	complete := bytes.LastIndexByte(chunk, '\n') + 1
	return parseLines(chunk[:complete]), offset + int64(complete), nil
}

func parseLines(data []byte) []model.AgentEvent {
	var events []model.AgentEvent
	for _, raw := range bytes.Split(data, []byte{'\n'}) {
		line := bytes.TrimSpace(raw) // also drops the '\r' of Windows line endings
		if len(line) == 0 {
			continue
		}
		if evt, err := model.ParseEvent(line); err == nil {
			events = append(events, evt)
		}
	}
	return events
}

// ReadEvents returns up to limit of the most recent events with timestamp >= since.
func (b *EventBus) ReadEvents(since float64, limit int) []model.AgentEvent {
	data, err := os.ReadFile(b.EventsFile)
	if err != nil {
		return nil
	}
	var events []model.AgentEvent
	for _, evt := range parseLines(data) {
		if evt.Timestamp >= since {
			events = append(events, evt)
		}
	}
	if limit >= 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	return events
}

// Latest returns the most recent event, from latest_event.json or the log.
func (b *EventBus) Latest() *model.AgentEvent {
	if data, err := os.ReadFile(b.LatestEvent); err == nil {
		if evt, err := model.ParseEvent(bytes.TrimSpace(data)); err == nil {
			return &evt
		}
	}
	events := b.ReadEvents(0, 1)
	if len(events) == 0 {
		return nil
	}
	return &events[0]
}

// Stream calls fn for each new event until ctx is done.
func (b *EventBus) Stream(ctx context.Context, poll time.Duration, fromStart bool, fn func(model.AgentEvent)) error {
	var offset int64
	if !fromStart {
		offset = b.EndOffset()
	}
	for {
		events, next, err := b.ReadFrom(offset)
		if err != nil {
			return err
		}
		offset = next
		for _, evt := range events {
			fn(evt)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(poll):
		}
	}
}
