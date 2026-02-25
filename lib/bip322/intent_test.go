package bip322

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestIntentValidate asserts Intent.Validate enforces required fields and
// metadata ordering.
func TestIntentValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		intent  *Intent
		wantErr string
	}{
		{
			name:    "nil_intent",
			intent:  nil,
			wantErr: "intent must be provided",
		},
		{
			name: "missing_payload",
			intent: &Intent{
				ValidFrom: 100,
			},
			wantErr: "intent payload must be provided",
		},
		{
			name: "invalid_window",
			intent: &Intent{
				Payload:    []byte("join-auth"),
				ValidFrom:  200,
				ValidUntil: 199,
			},
			wantErr: "valid-until block",
		},
		{
			name: "valid_open_ended",
			intent: &Intent{
				Payload:    []byte("join-auth"),
				ValidFrom:  10,
				ValidUntil: 0,
			},
		},
		{
			name: "valid_bounded",
			intent: &Intent{
				Payload:    []byte("join-auth"),
				ValidFrom:  10,
				ValidUntil: 20,
			},
		},
	}

	for i := 0; i < len(tests); i++ {
		testCase := tests[i]

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := testCase.intent.Validate()
			if testCase.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			require.Contains(t, err.Error(), testCase.wantErr)
		})
	}
}

// TestIntentValidateAtHeight asserts Intent.ValidateAtHeight enforces valid
// range checks against the current block height.
func TestIntentValidateAtHeight(t *testing.T) {
	t.Parallel()

	intent := &Intent{
		Payload:    []byte("join-auth"),
		ValidFrom:  100,
		ValidUntil: 200,
	}

	err := intent.ValidateAtHeight(99)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not yet valid")

	err = intent.ValidateAtHeight(100)
	require.NoError(t, err)

	err = intent.ValidateAtHeight(200)
	require.NoError(t, err)

	err = intent.ValidateAtHeight(201)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expired")
}

// TestIntentSigningMessageDeterministic asserts SigningMessage is stable for
// identical intent inputs.
func TestIntentSigningMessageDeterministic(t *testing.T) {
	t.Parallel()

	intent := &Intent{
		Payload:    []byte("deterministic payload"),
		ValidFrom:  123,
		ValidUntil: 456,
	}

	first, err := intent.SigningMessage()
	require.NoError(t, err)

	second, err := intent.SigningMessage()
	require.NoError(t, err)

	require.Equal(t, first, second)
}

// TestIntentSigningMessageCommitsMetadata asserts SigningMessage changes when
// intent metadata changes.
func TestIntentSigningMessageCommitsMetadata(t *testing.T) {
	t.Parallel()

	base := &Intent{
		Payload:    []byte("payload"),
		ValidFrom:  100,
		ValidUntil: 200,
	}

	baseMsg, err := base.SigningMessage()
	require.NoError(t, err)

	changedFrom := &Intent{
		Payload:    []byte("payload"),
		ValidFrom:  101,
		ValidUntil: 200,
	}

	changedFromMsg, err := changedFrom.SigningMessage()
	require.NoError(t, err)
	require.NotEqual(t, baseMsg, changedFromMsg)

	changedUntil := &Intent{
		Payload:    []byte("payload"),
		ValidFrom:  100,
		ValidUntil: 201,
	}

	changedUntilMsg, err := changedUntil.SigningMessage()
	require.NoError(t, err)
	require.NotEqual(t, baseMsg, changedUntilMsg)
}

// TestIntentSigningMessageTLVEncoding asserts SigningMessage uses the expected
// TLV envelope and commits all intent fields.
func TestIntentSigningMessageTLVEncoding(t *testing.T) {
	t.Parallel()

	intent := &Intent{
		Payload:    []byte("tlv-payload"),
		ValidFrom:  144,
		ValidUntil: 288,
	}

	raw, err := intent.SigningMessage()
	require.NoError(t, err)
	require.NotEmpty(t, raw)

	var (
		version    uint64
		domain     []byte
		validFrom  uint64
		validUntil uint64
		payload    []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			intentMessageVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			intentMessageDomainRecordType, &domain,
		),
		tlv.MakePrimitiveRecord(
			intentMessageValidFromRecordType, &validFrom,
		),
		tlv.MakePrimitiveRecord(
			intentMessageValidUntilRecordType, &validUntil,
		),
		tlv.MakePrimitiveRecord(
			intentMessagePayloadRecordType, &payload,
		),
	)
	require.NoError(t, err)

	reader := bytes.NewReader(raw)
	parsedTypes, err := stream.DecodeWithParsedTypes(reader)
	require.NoError(t, err)
	require.Equal(t, 0, reader.Len())

	_, ok := parsedTypes[intentMessageVersionRecordType]
	require.True(t, ok)

	_, ok = parsedTypes[intentMessageDomainRecordType]
	require.True(t, ok)

	_, ok = parsedTypes[intentMessageValidFromRecordType]
	require.True(t, ok)

	_, ok = parsedTypes[intentMessageValidUntilRecordType]
	require.True(t, ok)

	_, ok = parsedTypes[intentMessagePayloadRecordType]
	require.True(t, ok)

	require.Equal(t, intentMessageVersion, version)
	require.Equal(t, intentMessageDomainTag, string(domain))
	require.Equal(t, uint64(intent.ValidFrom), validFrom)
	require.Equal(t, uint64(intent.ValidUntil), validUntil)
	require.Equal(t, intent.Payload, payload)
}
