package actor

import (
	"bytes"
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestAskResponseEncodeDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response *AskResponse
	}{
		{
			name: "success with result",
			response: &AskResponse{
				CorrelationID: "corr-123",
				ResultBlob: []byte{
					0x01,
					0x02,
					0x03,
				},
				ErrorText: "",
			},
		},
		{
			name: "error response",
			response: &AskResponse{
				CorrelationID: "corr-456",
				ResultBlob:    []byte{},
				ErrorText:     "something went wrong",
			},
		},
		{
			name: "empty correlation id",
			response: &AskResponse{
				CorrelationID: "",
				ResultBlob: []byte{
					0xFF,
				},
				ErrorText: "",
			},
		},
		{
			name: "large result blob",
			response: &AskResponse{
				CorrelationID: "large-result",
				ResultBlob:    bytes.Repeat([]byte{0xAB}, 1000),
				ErrorText:     "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			err := tt.response.Encode(&buf)
			require.NoError(t, err)

			decoded := &AskResponse{}
			err = decoded.Decode(&buf)
			require.NoError(t, err)

			require.Equal(
				t, tt.response.CorrelationID,
				decoded.CorrelationID,
			)
			require.Equal(
				t, tt.response.ResultBlob, decoded.ResultBlob,
			)
			require.Equal(
				t, tt.response.ErrorText, decoded.ErrorText,
			)
		})
	}
}

func TestAskResponseMessageType(t *testing.T) {
	t.Parallel()

	response := &AskResponse{}
	require.Equal(t, "actor.AskResponse", response.MessageType())
}

func TestAskResponseTLVType(t *testing.T) {
	t.Parallel()

	response := &AskResponse{}
	require.Equal(t, AskResponseMsgType, response.TLVType())
}

func TestAskResponseIsError(t *testing.T) {
	t.Parallel()

	successResponse := &AskResponse{
		CorrelationID: "test",
		ResultBlob: []byte{
			0x01,
		},
		ErrorText: "",
	}
	require.False(t, successResponse.IsError())

	errorResponse := &AskResponse{
		CorrelationID: "test",
		ResultBlob:    nil,
		ErrorText:     "error message",
	}
	require.True(t, errorResponse.IsError())
}

func TestNewAskResponseSuccess(t *testing.T) {
	t.Parallel()

	correlationID := "corr-success"
	resultBlob := []byte{0x01, 0x02, 0x03}

	response := NewAskResponseSuccess(correlationID, resultBlob)

	require.Equal(t, correlationID, response.CorrelationID)
	require.Equal(t, resultBlob, response.ResultBlob)
	require.Empty(t, response.ErrorText)
	require.False(t, response.IsError())
}

func TestNewAskResponseError(t *testing.T) {
	t.Parallel()

	correlationID := "corr-error"
	errorText := "operation failed"

	response := NewAskResponseError(correlationID, errorText)

	require.Equal(t, correlationID, response.CorrelationID)
	require.Nil(t, response.ResultBlob)
	require.Equal(t, errorText, response.ErrorText)
	require.True(t, response.IsError())
}

func TestPropertyAskResponseRoundTrip(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		correlationID := rapid.String().Draw(rt, "correlationID")
		resultBlob := rapid.SliceOf(rapid.Byte()).Draw(rt, "resultBlob")
		errorText := rapid.String().Draw(rt, "errorText")

		original := &AskResponse{
			CorrelationID: correlationID,
			ResultBlob:    resultBlob,
			ErrorText:     errorText,
		}

		var buf bytes.Buffer
		err := original.Encode(&buf)
		require.NoError(rt, err)

		decoded := &AskResponse{}
		err = decoded.Decode(&buf)
		require.NoError(rt, err)

		require.Equal(rt, original.CorrelationID, decoded.CorrelationID)
		require.Equal(rt, original.ResultBlob, decoded.ResultBlob)
		require.Equal(rt, original.ErrorText, decoded.ErrorText)
	})
}

func TestPropertyAskResponseWithCodec(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(AskResponseMsgType, func() TLVMessage {
		return &AskResponse{}
	})

	rapid.Check(t, func(rt *rapid.T) {
		correlationID := rapid.String().Draw(rt, "correlationID")
		resultBlob := rapid.SliceOf(rapid.Byte()).Draw(rt, "resultBlob")

		original := NewAskResponseSuccess(correlationID, resultBlob)

		encoded, err := codec.Encode(original)
		require.NoError(rt, err)

		decoded, err := codec.Decode(encoded)
		require.NoError(rt, err)

		decodedResponse, ok := decoded.(*AskResponse)
		require.True(rt, ok)

		require.Equal(
			rt, original.CorrelationID,
			decodedResponse.CorrelationID,
		)
		require.Equal(
			rt, original.ResultBlob, decodedResponse.ResultBlob,
		)
		require.Equal(rt, original.ErrorText, decodedResponse.ErrorText)
	})
}

// testResultMessage is a simple TLVMessage for testing DecodeResult and
// NewAskResponseWithResult.
type testResultMessage struct {
	BaseMessage
	Value int64
}

func (m testResultMessage) MessageType() string {
	return "test.Result"
}

func (m testResultMessage) TLVType() tlv.Type {
	return 9999
}

func (m testResultMessage) Encode(w io.Writer) error {
	val := uint64(m.Value)
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(1, &val),
	}
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}

	return stream.Encode(w)
}

func (m *testResultMessage) Decode(r io.Reader) error {
	var val uint64
	records := []tlv.Record{
		tlv.MakePrimitiveRecord(1, &val),
	}
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return err
	}
	if _, err := stream.DecodeWithParsedTypes(r); err != nil {
		return err
	}
	m.Value = int64(val)

	return nil
}

func TestNewAskResponseWithResult(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(9999, func() TLVMessage {
		return &testResultMessage{}
	})

	t.Run("encodes result message correctly", func(t *testing.T) {
		t.Parallel()

		result := &testResultMessage{Value: 42}
		response, err := NewAskResponseWithResult(
			"corr-123", codec, result,
		)

		require.NoError(t, err)
		require.Equal(t, "corr-123", response.CorrelationID)
		require.NotEmpty(t, response.ResultBlob)
		require.Empty(t, response.ErrorText)
		require.False(t, response.IsError())
	})

	t.Run("result blob is decodable", func(t *testing.T) {
		t.Parallel()

		originalValue := int64(12345)
		result := &testResultMessage{Value: originalValue}
		response, err := NewAskResponseWithResult(
			"corr-456", codec, result,
		)
		require.NoError(t, err)

		// Decode the result blob using the codec.
		decoded, err := codec.Decode(response.ResultBlob)
		require.NoError(t, err)

		decodedResult, ok := decoded.(*testResultMessage)
		require.True(t, ok)
		require.Equal(t, originalValue, decodedResult.Value)
	})

	t.Run("works without registration since encode only needs TLVType",
		func(t *testing.T) {
			t.Parallel()

			// Encode doesn't require registration - only decode
			// does. The codec just calls msg.TLVType() and
			// msg.Encode().
			emptyCodec := NewMessageCodec()
			result := &testResultMessage{Value: 99}

			response, err := NewAskResponseWithResult(
				"corr-789", emptyCodec, result,
			)

			require.NoError(t, err)
			require.NotEmpty(t, response.ResultBlob)
		})
}

func TestDecodeResult(t *testing.T) {
	t.Parallel()

	codec := NewMessageCodec()
	codec.MustRegister(9999, func() TLVMessage {
		return &testResultMessage{}
	})

	t.Run("decodes success response", func(t *testing.T) {
		t.Parallel()

		// Create a response with an encoded result.
		originalValue := int64(999)
		result := &testResultMessage{Value: originalValue}
		response, err := NewAskResponseWithResult(
			"corr-decode-1", codec, result,
		)
		require.NoError(t, err)

		// Decode the result.
		decoded, err := response.DecodeResult(codec)
		require.NoError(t, err)

		decodedResult, ok := decoded.(*testResultMessage)
		require.True(t, ok)
		require.Equal(t, originalValue, decodedResult.Value)
	})

	t.Run("returns nil for empty result blob", func(t *testing.T) {
		t.Parallel()

		response := NewAskResponseSuccess("corr-empty", nil)

		decoded, err := response.DecodeResult(codec)

		require.NoError(t, err)
		require.Nil(t, decoded)
	})

	t.Run("returns error for error response", func(t *testing.T) {
		t.Parallel()

		response := NewAskResponseError(
			"corr-err", "something went wrong",
		)

		_, err := response.DecodeResult(codec)

		require.Error(t, err)
		require.Contains(t, err.Error(), "ask failed")
		require.Contains(t, err.Error(), "something went wrong")
	})

	t.Run("returns error for malformed blob", func(t *testing.T) {
		t.Parallel()

		// Create a response with garbage in the result blob.
		response := NewAskResponseSuccess(
			"corr-garbage", []byte{0xFF, 0xFF},
		)

		_, err := response.DecodeResult(codec)

		require.Error(t, err)
	})

	t.Run("returns error for unregistered type in blob", func(t *testing.T) {
		t.Parallel()

		// Create a valid response but use an empty codec for decoding.
		result := &testResultMessage{Value: 42}
		response, err := NewAskResponseWithResult(
			"corr-unreg", codec, result,
		)
		require.NoError(t, err)

		emptyCodec := NewMessageCodec()
		_, err = response.DecodeResult(emptyCodec)

		require.Error(t, err)
	})
}
