package arkrpc

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestGetInfoRequestOldShapeDecodesZero proves that a GetInfoRequest encoded
// without the new supported_ark_versions field (the pre-versioning "old
// shape") decodes under the new generated code with the new field left at its
// zero value. This is the additive-compatibility guarantee that lets an
// updated server decode a request from a pre-versioning client.
func TestGetInfoRequestOldShapeDecodesZero(t *testing.T) {
	t.Parallel()

	// The old shape is an empty request: the pre-versioning GetInfoRequest
	// carried no fields at all.
	oldShape := &GetInfoRequest{}

	raw, err := proto.Marshal(oldShape)
	require.NoError(t, err)

	decoded := &GetInfoRequest{}
	require.NoError(t, proto.Unmarshal(raw, decoded))

	// The newly added repeated field must be empty after decoding an old
	// message.
	require.Empty(t, decoded.SupportedArkVersions)
}

// TestGetInfoResponseOldShapeDecodesZero proves that a GetInfoResponse
// populated only with pre-versioning fields decodes under the new code with
// every newly added version field at its zero value. The updated client treats
// a zero selected version as incompatible and refuses startup.
func TestGetInfoResponseOldShapeDecodesZero(t *testing.T) {
	t.Parallel()

	// Populate only fields that existed before versioning was introduced.
	oldShape := &GetInfoResponse{
		Version: "0.1.0",
		Pubkey: []byte{
			0x02,
			0x03,
			0x04,
		},
		Network:           "regtest",
		BlockHeight:       100,
		BoardingExitDelay: 144,
		VtxoExitDelay:     144,
		DustLimit:         330,
	}

	raw, err := proto.Marshal(oldShape)
	require.NoError(t, err)

	decoded := &GetInfoResponse{}
	require.NoError(t, proto.Unmarshal(raw, decoded))

	// Existing fields round-trip unchanged.
	require.Equal(t, "0.1.0", decoded.Version)
	require.Equal(t, []byte{0x02, 0x03, 0x04}, decoded.Pubkey)
	require.Equal(t, "regtest", decoded.Network)

	// The newly added version fields must all be zero/empty because the
	// old shape never set them.
	require.Zero(t, decoded.SelectedArkVersion)
	require.Empty(t, decoded.ArkVersionPolicies)
	require.Zero(t, decoded.FreeRefreshWindowBlocks)
}

// TestGetInfoVersionFieldsRoundTrip proves that the new versioning fields are
// preserved across a marshal/unmarshal cycle, including the nested
// ArkVersionPolicy message and its State enum.
func TestGetInfoVersionFieldsRoundTrip(t *testing.T) {
	t.Parallel()

	req := &GetInfoRequest{
		SupportedArkVersions: []uint32{
			2,
			1,
		},
	}

	reqRaw, err := proto.Marshal(req)
	require.NoError(t, err)

	reqDecoded := &GetInfoRequest{}
	require.NoError(t, proto.Unmarshal(reqRaw, reqDecoded))
	require.Equal(t, []uint32{2, 1}, reqDecoded.SupportedArkVersions)

	v1Policy := &ArkVersionPolicy{
		Version: 1,
		State:   ArkVersionPolicy_STATE_DISABLED,
	}
	v2Policy := &ArkVersionPolicy{
		Version: 2,
		State:   ArkVersionPolicy_STATE_ACTIVE,
	}

	resp := &GetInfoResponse{
		Version:                 "1.0.0",
		SelectedArkVersion:      2,
		FreeRefreshWindowBlocks: 72,
		ArkVersionPolicies: []*ArkVersionPolicy{
			v1Policy,
			v2Policy,
		},
	}

	respRaw, err := proto.Marshal(resp)
	require.NoError(t, err)

	respDecoded := &GetInfoResponse{}
	require.NoError(t, proto.Unmarshal(respRaw, respDecoded))

	require.Equal(t, uint32(2), respDecoded.SelectedArkVersion)
	require.Equal(
		t, uint32(72), respDecoded.FreeRefreshWindowBlocks,
	)
	require.Len(t, respDecoded.ArkVersionPolicies, 2)

	first := respDecoded.ArkVersionPolicies[0]
	require.Equal(t, uint32(1), first.Version)
	require.Equal(t, ArkVersionPolicy_STATE_DISABLED, first.State)

	second := respDecoded.ArkVersionPolicies[1]
	require.Equal(t, uint32(2), second.Version)
	require.Equal(t, ArkVersionPolicy_STATE_ACTIVE, second.State)
}
