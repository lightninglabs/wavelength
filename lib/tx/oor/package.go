package oor

import (
	"encoding/json"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
)

// PackageVersion identifies the wire/storage encoding version for OOR packages.
type PackageVersion uint32

const (
	// PackageVersionV1 is the first versioned encoding of OOR
	// submit/finalize packages.
	PackageVersionV1 PackageVersion = 1
)

// SubmitPackage is the v0 OOR submit payload: an Ark tx PSBT plus checkpoint
// PSBTs.
//
// This is the shape we expect to send over RPC later. Today it is also the
// natural unit to persist durably and to hash/deduplicate.
type SubmitPackage struct {
	// ArkPSBT is the canonical Ark transaction PSBT.
	//
	// The v0 session identifier is derived from its unsigned txid.
	ArkPSBT *psbt.Packet

	// CheckpointPSBTs are the checkpoint transactions that the Ark tx
	// spends.
	CheckpointPSBTs []*psbt.Packet
}

// SessionID derives the stable v0 session identifier from the Ark txid.
func (p *SubmitPackage) SessionID() (chainhash.Hash, error) {
	if p == nil || p.ArkPSBT == nil || p.ArkPSBT.UnsignedTx == nil {
		return chainhash.Hash{}, fmt.Errorf("ark psbt must be provided")
	}

	return p.ArkPSBT.UnsignedTx.TxHash(), nil
}

// Validate applies the shared structural submit validation rules.
func (p *SubmitPackage) Validate() (*ValidatedSubmitPackage, error) {
	if p == nil {
		return nil, fmt.Errorf("submit package must be provided")
	}

	return ValidateSubmitPackage(p.ArkPSBT, p.CheckpointPSBTs)
}

// FinalizePackage is the v0 OOR finalize payload: the Ark tx PSBT plus
// finalized checkpoint PSBTs.
type FinalizePackage struct {
	// ArkPSBT is the canonical Ark transaction PSBT.
	ArkPSBT *psbt.Packet

	// FinalCheckpointPSBTs are the finalized checkpoint PSBTs.
	// They match the Ark tx inputs and include tap tree metadata.
	FinalCheckpointPSBTs []*psbt.Packet
}

// Validate applies the shared structural finalize validation rules.
func (p *FinalizePackage) Validate() error {
	if p == nil {
		return fmt.Errorf("finalize package must be provided")
	}

	return ValidateFinalizePackage(p.ArkPSBT, p.FinalCheckpointPSBTs)
}

type submitPackageJSONV1 struct {
	Version PackageVersion `json:"version"`

	// ArkPSBT is the base64-encoded PSBT serialization.
	ArkPSBT string `json:"ark_psbt_b64"`

	// Checkpoints is the base64-encoded PSBT serialization of each
	// checkpoint.
	Checkpoints []string `json:"checkpoint_psbts_b64"`
}

// MarshalSubmitPackage encodes a submit package as versioned JSON with base64
// PSBT payloads.
func MarshalSubmitPackage(pkg *SubmitPackage) ([]byte, error) {
	if pkg == nil {
		return nil, fmt.Errorf("submit package must be provided")
	}

	// We use JSON+base64 as a pragmatic "v0 durability encoding". This
	// makes it easy to persist on disk and to transport over future RPC
	// without committing to a binary schema before the protocol is
	// stabilized.
	//
	// Callers should still validate the package before use:
	// serialization is not a substitute for structural validation.
	arkB64, err := psbtutil.EncodeBase64(pkg.ArkPSBT)
	if err != nil {
		return nil, err
	}

	checkpoints := make([]string, 0, len(pkg.CheckpointPSBTs))
	for _, cp := range pkg.CheckpointPSBTs {
		cpB64, err := psbtutil.EncodeBase64(cp)
		if err != nil {
			return nil, err
		}

		checkpoints = append(checkpoints, cpB64)
	}

	payload := submitPackageJSONV1{
		Version:     PackageVersionV1,
		ArkPSBT:     arkB64,
		Checkpoints: checkpoints,
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal submit package: %w", err)
	}

	return b, nil
}

// UnmarshalSubmitPackage decodes a submit package previously encoded by
// MarshalSubmitPackage.
func UnmarshalSubmitPackage(b []byte) (*SubmitPackage, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("submit package bytes must be provided")
	}

	// We intentionally decode into a versioned struct so future protocol
	// upgrades can introduce new fields without silently changing behavior.
	var payload submitPackageJSONV1
	err := json.Unmarshal(b, &payload)
	if err != nil {
		return nil, fmt.Errorf("unmarshal submit package: %w", err)
	}

	if payload.Version != PackageVersionV1 {
		return nil, fmt.Errorf("unknown package version: %d",
			payload.Version)
	}

	ark, err := psbtutil.DecodeBase64(payload.ArkPSBT)
	if err != nil {
		return nil, err
	}

	checkpoints := make([]*psbt.Packet, 0, len(payload.Checkpoints))
	for _, cpB64 := range payload.Checkpoints {
		cp, err := psbtutil.DecodeBase64(cpB64)
		if err != nil {
			return nil, err
		}

		checkpoints = append(checkpoints, cp)
	}

	return &SubmitPackage{
		ArkPSBT:         ark,
		CheckpointPSBTs: checkpoints,
	}, nil
}
