package oor

import "fmt"

// Package-wide bounds on externally-submitted OOR payloads. These are
// intentionally conservative defaults chosen to match client-side
// expectations; tune them under production metrics (see the tracking
// issue) rather than adjusting them from incident reports.
const (
	// MaxCheckpointPSBTsPerRequest caps how many checkpoint PSBTs a
	// single Submit/Finalize request may carry. The client-side
	// darepo-client caps its produced checkpoint list at the same
	// value, so a well-behaved client never trips this bound.
	MaxCheckpointPSBTsPerRequest = 64

	// MaxVTXOSigningDescriptorsPerRequest caps the number of signing
	// descriptors the server will parse per submit. Each descriptor
	// drives a DB read and an arkscript rebuild downstream, so this
	// bound is the first line of defense against per-request
	// amplification.
	MaxVTXOSigningDescriptorsPerRequest = MaxCheckpointPSBTsPerRequest

	// MaxRecipientOutputsPerRequest caps the number of recipient
	// output metadata records carried on the submit request. Each
	// recipient contributes an arkscript policy template decode and a
	// pkScript binding check.
	MaxRecipientOutputsPerRequest = MaxCheckpointPSBTsPerRequest

	// MaxPSBTBytesPerRequest caps the serialized size of any single
	// PSBT blob parsed from a Submit or Finalize payload. PSBTs
	// larger than this are rejected at the TLV / deserialize
	// boundary, before the psbt library allocates internal state.
	MaxPSBTBytesPerRequest = 64 * 1024
)

// enforceSubmitRequestLimits validates the size-bounded fields of an
// incoming SubmitOORRequest against the caps above. Running this at
// the top of handleSubmit lets the expensive validation/rebuild
// pipeline assume bounded inputs.
func enforceSubmitRequestLimits(msg *SubmitOORRequest) error {
	if msg == nil {
		return nil
	}

	if n := len(msg.CheckpointPSBTs); n > MaxCheckpointPSBTsPerRequest {
		return fmt.Errorf(
			"submit carries %d checkpoint PSBTs; max allowed %d",
			n, MaxCheckpointPSBTsPerRequest,
		)
	}
	if n := len(msg.VTXOSigningDescriptors); n >
		MaxVTXOSigningDescriptorsPerRequest {

		return fmt.Errorf(
			"submit carries %d signing descriptors; max allowed %d",
			n, MaxVTXOSigningDescriptorsPerRequest,
		)
	}
	if n := len(msg.Recipients); n > MaxRecipientOutputsPerRequest {
		return fmt.Errorf(
			"submit carries %d recipients; max allowed %d",
			n, MaxRecipientOutputsPerRequest,
		)
	}

	return nil
}

// enforceFinalizeRequestLimits validates the size-bounded fields of
// an incoming FinalizeOORRequest.
func enforceFinalizeRequestLimits(msg *FinalizeOORRequest) error {
	if msg == nil {
		return nil
	}

	if n := len(msg.FinalCheckpointPSBTs); n >
		MaxCheckpointPSBTsPerRequest {

		return fmt.Errorf(
			"finalize carries %d checkpoint PSBTs; max allowed %d",
			n, MaxCheckpointPSBTsPerRequest,
		)
	}

	return nil
}
