//go:build systest

package systest

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"
	"github.com/lightninglabs/darepo/clientconn"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// MessageDirection indicates the direction of a message.
type MessageDirection int

const (
	// ClientToServer indicates a message from client to server.
	ClientToServer MessageDirection = iota

	// ServerToClient indicates a message from server to client.
	ServerToClient
)

// String returns a human-readable representation of the direction.
func (d MessageDirection) String() string {
	switch d {
	case ClientToServer:
		return "C2S"

	case ServerToClient:
		return "S2C"

	default:
		return "UNKNOWN"
	}
}

// TranscriptEntry records a single message in the communication transcript.
type TranscriptEntry struct {
	// Timestamp is when the message was recorded.
	Timestamp time.Time

	// Direction indicates whether the message was client→server or
	// server→client.
	Direction MessageDirection

	// ClientID identifies which client sent or received the message.
	ClientID clientconn.ClientID

	// MsgType is the type name of the message (e.g., "JoinRoundRequest").
	MsgType string

	// Msg is the actual message payload.
	Msg any

	// Envelope stores the original mailbox envelope for transcript entries
	// recorded from mailbox traffic.
	Envelope *mailboxpb.Envelope
}

// MessageTranscript records all messages for test assertions.
type MessageTranscript struct {
	mu      sync.Mutex
	entries []TranscriptEntry
}

// NewMessageTranscript creates a new empty transcript.
func NewMessageTranscript() *MessageTranscript {
	return &MessageTranscript{
		entries: make([]TranscriptEntry, 0),
	}
}

// Record adds a message to the transcript.
func (t *MessageTranscript) Record(dir MessageDirection,
	clientID clientconn.ClientID, msg any) {

	t.mu.Lock()
	defer t.mu.Unlock()

	// Extract the type name from the message.
	msgType := reflect.TypeOf(msg).String()

	// Remove the pointer prefix if present.
	if len(msgType) > 0 && msgType[0] == '*' {
		msgType = msgType[1:]
	}

	// Extract just the type name without package path.
	for i := len(msgType) - 1; i >= 0; i-- {
		if msgType[i] == '.' {
			msgType = msgType[i+1:]
			break
		}
	}

	t.entries = append(t.entries, TranscriptEntry{
		Timestamp: time.Now(),
		Direction: dir,
		ClientID:  clientID,
		MsgType:   msgType,
		Msg:       msg,
	})
}

// RecordEnvelope adds a transcript entry backed by a mailbox envelope. This is
// used by the InstrumentedMailbox which records from proto envelopes.
func (t *MessageTranscript) RecordEnvelope(dir MessageDirection,
	clientID clientconn.ClientID, typeName string,
	env *mailboxpb.Envelope) {

	t.mu.Lock()
	defer t.mu.Unlock()

	var clone *mailboxpb.Envelope
	if env != nil {
		clone = proto.Clone(env).(*mailboxpb.Envelope)
	}

	t.entries = append(t.entries, TranscriptEntry{
		Timestamp: time.Now(),
		Direction: dir,
		ClientID:  clientID,
		MsgType:   typeName,
		Msg:       nil,
		Envelope:  clone,
	})
}

// Entries returns a copy of all transcript entries.
func (t *MessageTranscript) Entries() []TranscriptEntry {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := make([]TranscriptEntry, len(t.entries))
	copy(result, t.entries)

	return result
}

// Clear removes all entries from the transcript.
func (t *MessageTranscript) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.entries = t.entries[:0]
}

// ExpectedMessage describes an expected message in the transcript.
type ExpectedMessage struct {
	// Direction indicates whether we expect client→server or
	// server→client.
	Direction MessageDirection

	// MsgType is the expected type name of the message.
	MsgType string

	// ClientID optionally specifies which client the message should be
	// from/to. If empty, any client matches.
	ClientID clientconn.ClientID
}

// C2S creates an expected client-to-server message.
func C2S(msgType string) ExpectedMessage {
	return ExpectedMessage{
		Direction: ClientToServer,
		MsgType:   msgType,
	}
}

// C2SFrom creates an expected client-to-server message from a specific client.
func C2SFrom(msgType string, clientID clientconn.ClientID) ExpectedMessage {
	return ExpectedMessage{
		Direction: ClientToServer,
		MsgType:   msgType,
		ClientID:  clientID,
	}
}

// S2C creates an expected server-to-client message.
func S2C(msgType string) ExpectedMessage {
	return ExpectedMessage{
		Direction: ServerToClient,
		MsgType:   msgType,
	}
}

// S2CTo creates an expected server-to-client message to a specific client.
func S2CTo(msgType string, clientID clientconn.ClientID) ExpectedMessage {
	return ExpectedMessage{
		Direction: ServerToClient,
		MsgType:   msgType,
		ClientID:  clientID,
	}
}

// AssertMessageSequence verifies the full client+server message sequence.
func (t *MessageTranscript) AssertMessageSequence(tb testing.TB,
	expected []ExpectedMessage) {

	tb.Helper()

	entries := t.Entries()

	require.Len(
		tb, entries, len(expected),
		"transcript length mismatch: got %d entries, expected %d",
		len(entries), len(expected),
	)

	for i, exp := range expected {
		entry := entries[i]

		require.Equal(
			tb, exp.Direction, entry.Direction,
			"message %d direction mismatch: got %s, expected %s",
			i, entry.Direction, exp.Direction,
		)

		require.Equal(
			tb, exp.MsgType, entry.MsgType,
			"message %d type mismatch: got %s, expected %s",
			i, entry.MsgType, exp.MsgType,
		)

		// Only check client ID if specified in the expected message.
		if exp.ClientID != "" {
			require.Equal(
				tb, exp.ClientID, entry.ClientID,
				"message %d client ID mismatch: got %s, "+
					"expected %s",
				i, entry.ClientID, exp.ClientID,
			)
		}
	}
}

// AssertClientReceivedTypes verifies a client received specific message types.
func (t *MessageTranscript) AssertClientReceivedTypes(tb testing.TB,
	clientID clientconn.ClientID, expectedTypes []string) {

	tb.Helper()

	entries := t.Entries()

	var receivedTypes []string
	for _, entry := range entries {
		if entry.Direction == ServerToClient &&
			entry.ClientID == clientID {

			receivedTypes = append(receivedTypes, entry.MsgType)
		}
	}

	require.Equal(
		tb, expectedTypes, receivedTypes,
		"client %s received types mismatch", clientID,
	)
}

// AssertClientSentTypes verifies a client sent specific message types.
func (t *MessageTranscript) AssertClientSentTypes(tb testing.TB,
	clientID clientconn.ClientID, expectedTypes []string) {

	tb.Helper()

	entries := t.Entries()

	var sentTypes []string
	for _, entry := range entries {
		if entry.Direction == ClientToServer &&
			entry.ClientID == clientID {

			sentTypes = append(sentTypes, entry.MsgType)
		}
	}

	require.Equal(
		tb, expectedTypes, sentTypes,
		"client %s sent types mismatch", clientID,
	)
}

// AssertContainsMessage verifies the transcript contains a message of the given
// type and direction.
func (t *MessageTranscript) AssertContainsMessage(tb testing.TB,
	expected ExpectedMessage) {

	tb.Helper()

	entries := t.Entries()

	for _, entry := range entries {
		if entry.Direction != expected.Direction {
			continue
		}
		if entry.MsgType != expected.MsgType {
			continue
		}
		if expected.ClientID != "" &&
			entry.ClientID != expected.ClientID {

			continue
		}

		// Found a match.
		return
	}

	require.Fail(
		tb,
		fmt.Sprintf(
			"transcript does not contain expected message: %s %s",
			expected.Direction, expected.MsgType,
		),
	)
}

// AssertNotContainsMessage verifies the transcript does NOT contain a message
// of the given type and direction. This is useful for asserting that an error
// response was NOT sent.
func (t *MessageTranscript) AssertNotContainsMessage(tb testing.TB,
	unexpected ExpectedMessage) {

	tb.Helper()

	entries := t.Entries()

	for _, entry := range entries {
		if entry.Direction != unexpected.Direction {
			continue
		}
		if entry.MsgType != unexpected.MsgType {
			continue
		}
		if unexpected.ClientID != "" &&
			entry.ClientID != unexpected.ClientID {

			continue
		}

		// Found a match - this is a failure.
		require.Fail(
			tb,
			fmt.Sprintf(
				"transcript unexpectedly contains "+
					"message: %s %s (client: %s)",
				unexpected.Direction, unexpected.MsgType,
				entry.ClientID,
			),
		)
	}
}

// WaitForEntryCount waits for the transcript to have at least the specified
// number of entries, with a timeout.
func (t *MessageTranscript) WaitForEntryCount(count int,
	timeout time.Duration) error {

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		t.mu.Lock()
		current := len(t.entries)
		t.mu.Unlock()

		if current >= count {
			return nil
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.mu.Lock()
	current := len(t.entries)
	t.mu.Unlock()

	return fmt.Errorf(
		"timeout waiting for %d entries, got %d", count, current,
	)
}

// GetLastEntryOfType returns the most recent entry matching the given direction
// and type, or nil if not found.
func (t *MessageTranscript) GetLastEntryOfType(dir MessageDirection,
	msgType string) *TranscriptEntry {

	t.mu.Lock()
	defer t.mu.Unlock()

	for i := len(t.entries) - 1; i >= 0; i-- {
		entry := t.entries[i]
		if entry.Direction == dir && entry.MsgType == msgType {
			return &entry
		}
	}

	return nil
}

// Dump returns a human-readable representation of the transcript for debugging.
func (t *MessageTranscript) Dump() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	var result string
	for i, entry := range t.entries {
		result += fmt.Sprintf(
			"%d. [%s] %s %s (client: %s)\n",
			i+1, entry.Timestamp.Format("15:04:05.000"),
			entry.Direction, entry.MsgType, entry.ClientID,
		)
	}

	return result
}
