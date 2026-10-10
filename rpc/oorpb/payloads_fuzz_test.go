package oorpb

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// The parsers in this file (ParseSubmitPackageRequest and
// ParseFinalizePackageRequest) run on the SERVER's OOR RPC handlers
// against fully client-controlled proto bytes. A panic, OOM, slice
// out-of-bounds, or integer overflow anywhere in these decode paths is a
// remote server-crash vector: a single hostile client could take down
// the operator. These fuzz targets assert ONLY the no-panic invariant
// (the parsers are free to return errors); they are deliberately not
// round-trip checks. Seeds cover a representative valid value, empty
// input, and off-length edge cases (31/33-byte session hashes) that
// historically trip fixed-length copies.

// validSessionID is a representative 32-byte session hash used to seed the
// fuzzers with a value that passes the length gate so the fuzzer explores
// past it.
var validSessionID = []byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// FuzzParseSubmitPackageRequestFields builds a SubmitPackageRequest in
// the harness from fuzzer-controlled bytes for every byte-typed field the
// parser touches (ark PSBT, one checkpoint PSBT entry, one descriptor's
// outpoint txid + policy/spend-path bytes, one recipient's pk_script and
// value). This drives the decode helpers directly without the proto wire
// layer in the way, so the fuzzer can reach the inner length checks
// (decodeOutPoint's 32-byte txid gate, psbtutil.Parse) quickly.
func FuzzParseSubmitPackageRequestFields(f *testing.F) {
	// Seed: a plausible-shaped request. The PSBTs will fail to parse,
	// but the goal is no-panic, not success.
	f.Add(
		[]byte("psbt"), []byte("checkpoint"), validSessionID,
		[]byte("policy"), []byte("spendpath"), uint32(0),
		[]byte{0x51}, int64(1000), []byte("vtxopolicy"),
	)
	// Seed: all-empty fields (proto defaults).
	f.Add(
		[]byte{}, []byte{}, []byte{}, []byte{}, []byte{},
		uint32(0), []byte{}, int64(0), []byte{},
	)
	// Seed: off-length txid (31 bytes) to exercise the length gate in
	// decodeOutPoint, plus a negative recipient value.
	f.Add(
		[]byte{}, []byte{}, validSessionID[:31], []byte{},
		[]byte{}, uint32(0xffffffff), []byte{}, int64(-1), []byte{},
	)
	// Seed: off-length txid (33 bytes).
	f.Add(
		[]byte{}, []byte{}, append(append([]byte{}, validSessionID...),
			0xaa), []byte{}, []byte{}, uint32(1), []byte{},
		int64(1<<62), []byte{},
	)

	f.Fuzz(func(t *testing.T, arkPSBT, checkpointPSBT, descTxid,
		descPolicy, descSpendPath []byte, descVout uint32,
		recipientScript []byte, recipientValue int64,
		recipientPolicy []byte) {

		req := &SubmitPackageRequest{
			ArkPsbt:         arkPSBT,
			CheckpointPsbts: [][]byte{checkpointPSBT},
			SigningDescriptors: []*OORSigningDescriptor{
				{
					Outpoint: &OOROutPoint{
						Txid: descTxid,
						Vout: descVout,
					},
					VtxoPolicyTemplate: descPolicy,
					SpendPath:          descSpendPath,
					OwnerLeafPolicy:    descPolicy,
				},
				// A nil descriptor entry must be rejected, not
				// dereferenced.
				nil,
			},
			RecipientOutputs: []*OORRecipientOutput{
				{
					PkScript:           recipientScript,
					ValueSat:           recipientValue,
					VtxoPolicyTemplate: recipientPolicy,
				},
				nil,
			},
		}

		// Only assertion: the parser must not panic. Returning an
		// error is the expected behavior for hostile input.
		_, _, _, _, _ = ParseSubmitPackageRequest(req)
	})
}

// FuzzParseSubmitPackageRequestWire feeds raw bytes straight into
// proto.Unmarshal and then into the parser. This catches panics that only
// manifest from wire shapes the in-harness field fuzzer cannot construct
// (e.g. many repeated descriptors/recipients, deeply nested unknown
// fields). The seed is a marshaled valid-shaped request.
func FuzzParseSubmitPackageRequestWire(f *testing.F) {
	seed := &SubmitPackageRequest{
		ArkPsbt:         []byte("psbt"),
		CheckpointPsbts: [][]byte{[]byte("a"), []byte("b")},
		SigningDescriptors: []*OORSigningDescriptor{
			{
				Outpoint: &OOROutPoint{
					Txid: validSessionID,
					Vout: 0,
				},
				VtxoPolicyTemplate: []byte("policy"),
				SpendPath:          []byte("spend"),
				OwnerLeafPolicy:    []byte("owner"),
			},
		},
		RecipientOutputs: []*OORRecipientOutput{
			{
				PkScript: []byte{0x51},
				ValueSat: 1000,
			},
		},
	}
	if raw, err := proto.Marshal(seed); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var req SubmitPackageRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			return
		}

		_, _, _, _, _ = ParseSubmitPackageRequest(&req)
	})
}

// FuzzParseFinalizePackageRequestFields builds a FinalizePackageRequest
// from a fuzzer-controlled session id and one checkpoint PSBT entry. The
// session id is the prime target: decodeSessionID does a fixed 32-byte
// copy gated by a length check, and an off-by-one in that gate would be a
// slice OOB.
func FuzzParseFinalizePackageRequestFields(f *testing.F) {
	f.Add(validSessionID, []byte("checkpoint"))
	f.Add([]byte{}, []byte{})
	// 31-byte and 33-byte session ids straddle the length gate.
	f.Add(validSessionID[:31], []byte{})
	f.Add(append(append([]byte{}, validSessionID...), 0xaa), []byte{})

	f.Fuzz(func(t *testing.T, sessionID, checkpointPSBT []byte) {
		req := &FinalizePackageRequest{
			SessionId:            sessionID,
			FinalCheckpointPsbts: [][]byte{checkpointPSBT},
		}

		_, _, _ = ParseFinalizePackageRequest(req)
	})
}

// FuzzParseFinalizePackageRequestWire feeds raw bytes through
// proto.Unmarshal, then the parser, to probe wire shapes (e.g. a large
// repeated final_checkpoint_psbts) beyond the field fuzzer's reach.
func FuzzParseFinalizePackageRequestWire(f *testing.F) {
	seed := &FinalizePackageRequest{
		SessionId:            validSessionID,
		FinalCheckpointPsbts: [][]byte{[]byte("a")},
	}
	if raw, err := proto.Marshal(seed); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var req FinalizePackageRequest
		if err := proto.Unmarshal(data, &req); err != nil {
			return
		}

		_, _, _ = ParseFinalizePackageRequest(&req)
	})
}

// FuzzParseSubmitPackageResponse exercises the response parser. Although
// the response is operator-authored on the happy path, the same decode
// helpers (decodeSessionID, psbtutil.Parse, decodePSBTSlice) run on it,
// and a compromised or buggy operator must not be able to panic the
// client. Wire-bytes style covers both the success and rejection oneof
// branches.
func FuzzParseSubmitPackageResponse(f *testing.F) {
	success := &SubmitPackageResponse{
		Result: &SubmitPackageResponse_Success{
			Success: &SubmitPackageSuccess{
				SessionId:               validSessionID,
				CoSignedArkPsbt:         []byte("psbt"),
				CoSignedCheckpointPsbts: [][]byte{[]byte("a")},
			},
		},
	}
	if raw, err := proto.Marshal(success); err == nil {
		f.Add(raw)
	}
	rejection := &SubmitPackageResponse{
		Result: &SubmitPackageResponse_Rejection{
			Rejection: &SubmitPackageRejection{
				SessionId: validSessionID,
				Reason:    "nope",
			},
		},
	}
	if raw, err := proto.Marshal(rejection); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var resp SubmitPackageResponse
		if err := proto.Unmarshal(data, &resp); err != nil {
			return
		}

		_, _, _, _ = ParseSubmitPackageResponse(&resp)
	})
}

// FuzzParseFinalizePackageResponse exercises decodeSessionID on the
// finalize response's session_id via the wire layer.
func FuzzParseFinalizePackageResponse(f *testing.F) {
	seed := &FinalizePackageResponse{SessionId: validSessionID}
	if raw, err := proto.Marshal(seed); err == nil {
		f.Add(raw)
	}
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		var resp FinalizePackageResponse
		if err := proto.Unmarshal(data, &resp); err != nil {
			return
		}

		_, _ = ParseFinalizePackageResponse(&resp)
	})
}
