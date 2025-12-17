package actor

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TLV type definitions for test message fields.
type tlvValue = tlv.RecordT[tlv.TlvType1, uint64]
type tlvCounter = tlv.RecordT[tlv.TlvType2, uint32]
type tlvOptional = tlv.RecordT[tlv.TlvType3, []byte]

const testTLVMsgType tlv.Type = 0x1000

// testTLVMsg is a test message that implements TLVMessage using RecordT.
type testTLVMsg struct {
	BaseMessage
	Value    tlvValue
	Counter  tlvCounter
	Optional tlvOptional
}

func (m *testTLVMsg) MessageType() string {
	return "test.TLVMsg"
}

func (m *testTLVMsg) TLVType() tlv.Type {
	return testTLVMsgType
}

// Encode serializes the test message as a TLV stream.
func (m *testTLVMsg) Encode(w io.Writer) error {
	records := []tlv.Record{
		m.Value.Record(),
		m.Counter.Record(),
		m.Optional.Record(),
	}

	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

// Decode deserializes a TLV stream into the test message.
func (m *testTLVMsg) Decode(r io.Reader) error {
	stream, err := tlv.NewStream(
		m.Value.Record(),
		m.Counter.Record(),
		m.Optional.Record(),
	)
	if err != nil {
		return err
	}

	_, err = stream.DecodeWithParsedTypes(r)

	return err
}

// newTestTLVMsg creates a new test message constructor.
func newTestTLVMsg() TLVMessage {
	return &testTLVMsg{}
}

// TestMessageCodecRegister tests that message types can be registered.
func TestMessageCodecRegister(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()

	// First registration should succeed.
	err := codec.Register(testTLVMsgType, newTestTLVMsg)
	require.NoError(t, err)

	// Second registration of same type should fail.
	err = codec.Register(testTLVMsgType, newTestTLVMsg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "already registered")

	// Different type should succeed.
	err = codec.Register(0x1001, newTestTLVMsg)
	require.NoError(t, err)
}

// TestMessageCodecMustRegister tests that MustRegister panics on duplicate.
func TestMessageCodecMustRegister(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(testTLVMsgType, newTestTLVMsg)

	// Second registration should panic.
	require.Panics(t, func() {
		codec.MustRegister(testTLVMsgType, newTestTLVMsg)
	})
}

// TestMessageCodecEncodeDecode tests round-trip encoding/decoding.
func TestMessageCodecEncodeDecode(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(testTLVMsgType, newTestTLVMsg)

	original := &testTLVMsg{
		Value:    tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		Counter:  tlv.NewPrimitiveRecord[tlv.TlvType2](uint32(100)),
		Optional: tlv.NewPrimitiveRecord[tlv.TlvType3]([]byte{1, 2, 3, 4}),
	}

	// Encode.
	data, err := codec.Encode(original)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Decode.
	decoded, err := codec.Decode(data)
	require.NoError(t, err)

	decodedMsg, ok := decoded.(*testTLVMsg)
	require.True(t, ok, "decoded should be *testTLVMsg")

	// Verify fields.
	require.Equal(t, original.Value.Val, decodedMsg.Value.Val)
	require.Equal(t, original.Counter.Val, decodedMsg.Counter.Val)
	require.Equal(t, original.Optional.Val, decodedMsg.Optional.Val)
}

// TestMessageCodecDecodeUnknownType tests that decoding unknown types fails.
func TestMessageCodecDecodeUnknownType(t *testing.T) {
	t.Parallel()

	// Create two codecs - one with the type registered, one without.
	encoderCodec := NewMessageCodec()
	encoderCodec.MustRegister(testTLVMsgType, newTestTLVMsg)

	decoderCodec := NewMessageCodec() // Empty registry.

	msg := &testTLVMsg{
		Value: tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(123)),
	}
	data, err := encoderCodec.Encode(msg)
	require.NoError(t, err)

	// Decoding with empty registry should fail.
	_, err = decoderCodec.Decode(data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown message type")
}

// TestMessageCodecIsRegistered tests the IsRegistered method.
func TestMessageCodecIsRegistered(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()

	require.False(t, codec.IsRegistered(testTLVMsgType))

	codec.MustRegister(testTLVMsgType, newTestTLVMsg)

	require.True(t, codec.IsRegistered(testTLVMsgType))
	require.False(t, codec.IsRegistered(0x9999))
}

// TestMessageCodecRegisteredTypes tests listing registered types.
func TestMessageCodecRegisteredTypes(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()

	require.Empty(t, codec.RegisteredTypes())

	codec.MustRegister(0x1000, newTestTLVMsg)
	codec.MustRegister(0x1001, newTestTLVMsg)
	codec.MustRegister(0x1002, newTestTLVMsg)

	types := codec.RegisteredTypes()
	require.Len(t, types, 3)
}

// TestMessageCodecEncodeEmptyMessage tests encoding a message with zero values.
func TestMessageCodecEncodeEmptyMessage(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(testTLVMsgType, newTestTLVMsg)

	// Message with all zero/empty values.
	original := &testTLVMsg{}

	data, err := codec.Encode(original)
	require.NoError(t, err)

	decoded, err := codec.Decode(data)
	require.NoError(t, err)

	decodedMsg := decoded.(*testTLVMsg)
	require.Equal(t, uint64(0), decodedMsg.Value.Val)
	require.Equal(t, uint32(0), decodedMsg.Counter.Val)
	require.Empty(t, decodedMsg.Optional.Val)
}

// TestMessageCodecConcurrentAccess tests thread safety of the codec.
func TestMessageCodecConcurrentAccess(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(testTLVMsgType, newTestTLVMsg)

	msg := &testTLVMsg{
		Value:   tlv.NewPrimitiveRecord[tlv.TlvType1](uint64(42)),
		Counter: tlv.NewPrimitiveRecord[tlv.TlvType2](uint32(100)),
	}

	// Run concurrent encode/decode operations.
	done := make(chan struct{})
	for i := 0; i < 10; i++ {
		go func() {
			defer func() { done <- struct{}{} }()

			for j := 0; j < 100; j++ {
				data, err := codec.Encode(msg)
				require.NoError(t, err)

				_, err = codec.Decode(data)
				require.NoError(t, err)
			}
		}()
	}

	// Wait for all goroutines.
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestMessageCodecRapidRoundTrip is a property-based test for round-trip
// encoding/decoding.
func TestMessageCodecRapidRoundTrip(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(testTLVMsgType, newTestTLVMsg)

	rapid.Check(t, func(t *rapid.T) {
		value := rapid.Uint64().Draw(t, "value")
		counter := rapid.Uint32().Draw(t, "counter")
		optional := rapid.SliceOf(rapid.Byte()).Draw(t, "optional")

		original := &testTLVMsg{
			Value:    tlv.NewPrimitiveRecord[tlv.TlvType1](value),
			Counter:  tlv.NewPrimitiveRecord[tlv.TlvType2](counter),
			Optional: tlv.NewPrimitiveRecord[tlv.TlvType3](optional),
		}

		// Encode.
		data, err := codec.Encode(original)
		require.NoError(t, err)

		// Decode.
		decoded, err := codec.Decode(data)
		require.NoError(t, err)

		decodedMsg := decoded.(*testTLVMsg)

		// Verify.
		require.Equal(t, original.Value.Val, decodedMsg.Value.Val)
		require.Equal(t, original.Counter.Val, decodedMsg.Counter.Val)
		require.Equal(t, original.Optional.Val, decodedMsg.Optional.Val)
	})
}

// TestMessageCodecDecodeCorruptedData tests handling of corrupted data.
func TestMessageCodecDecodeCorruptedData(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(testTLVMsgType, newTestTLVMsg)

	testCases := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"truncated type id", []byte{0xFF}},
		{"truncated length", []byte{0x00, 0x10}},
		{"invalid varint", []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := codec.Decode(tc.data)
			require.Error(t, err)
		})
	}
}
