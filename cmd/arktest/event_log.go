//go:build itest

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// defaultEventLogName is the sparse JSON-lines event artifact name.
const defaultEventLogName = "events.jsonl"

// eventLog prints sparse, timestamped arktest events to the terminal and, when
// configured, mirrors the structured event stream to a JSON-lines artifact.
type eventLog struct {
	out     io.Writer
	file    *os.File
	history []eventRecord
}

// eventRecord is the stable JSON-lines shape written by eventLog.
type eventRecord struct {
	Time    string         `json:"time"`
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// newEventLog opens an event logger. If path is empty, only terminal output is
// produced.
func newEventLog(out io.Writer, path string) (*eventLog, error) {
	l := &eventLog{out: out}
	if path == "" {
		return l, nil
	}

	f, err := os.OpenFile(
		path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	l.file = f

	return l, nil
}

// AttachFile opens the JSON-lines artifact and flushes any events printed
// before the final run directory was known.
func (l *eventLog) AttachFile(path string) error {
	if l == nil || path == "" {
		return nil
	}
	if l.file != nil {
		return nil
	}

	f, err := os.OpenFile(
		path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600,
	)
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	l.file = f

	for _, rec := range l.history {
		if err := l.writeRecord(rec); err != nil {
			return err
		}
	}
	l.history = nil

	return nil
}

// Close closes the underlying JSON-lines artifact, if one was opened.
func (l *eventLog) Close() error {
	if l == nil || l.file == nil {
		return nil
	}

	return l.file.Close()
}

// Print records a high-level arktest event with optional structured fields.
func (l *eventLog) Print(kind, msg string, fields map[string]any) {
	if l == nil {
		return
	}

	now := time.Now()
	fmt.Fprintf(l.out, "[%s] %s\n", now.Format("15:04:05.000"), msg)

	rec := eventRecord{
		Time:    now.UTC().Format(time.RFC3339Nano),
		Kind:    kind,
		Message: msg,
		Fields:  fields,
	}
	if l.file == nil {
		l.history = append(l.history, rec)
		return
	}

	if err := l.writeRecord(rec); err != nil {
		fmt.Fprintf(l.out, "[%s] event marshal failed: %v\n",
			now.Format("15:04:05.000"), err)
	}
}

// Printf records a formatted high-level arktest event.
func (l *eventLog) Printf(kind string, fields map[string]any,
	format string, args ...any) {

	l.Print(kind, fmt.Sprintf(format, args...), fields)
}

// writeRecord appends one structured event to the JSON-lines artifact.
func (l *eventLog) writeRecord(rec eventRecord) error {
	buf, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	_, err = l.file.Write(append(buf, '\n'))

	return err
}
