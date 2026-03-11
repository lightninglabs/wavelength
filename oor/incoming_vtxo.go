package oor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/scripts"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/lib/tx/arktx"
	"github.com/lightninglabs/darepo-client/vtxo"
	"github.com/lightningnetwork/lnd/keychain"
)

// IncomingVTXOMetadata carries authoritative lineage/expiry metadata for an
// incoming OOR VTXO. The receive path must not invent synthetic placeholders
// for these fields because they drive expiry logic and unilateral-exit lineage.
type IncomingVTXOMetadata struct {
	// RoundID identifies the round lineage this VTXO belongs to.
	RoundID string

	// CommitmentTxID is the commitment transaction anchoring this VTXO
	// lineage.
	CommitmentTxID chainhash.Hash

	// BatchExpiry is the absolute batch expiry height.
	BatchExpiry int32

	// TreeDepth is the VTXO depth in the commitment tree.
	TreeDepth int

	// ChainDepth is the number of OOR checkpoint hops between this
	// VTXO and the last on-chain commitment. This is distinct from
	// TreeDepth, which tracks position in the VTXT.
	ChainDepth int

	// CreatedHeight is the block height at which the VTXO was created.
	CreatedHeight int32

	// TreePath is the minimal inclusion path used for unilateral exit.
	TreePath *tree.Tree
}

// IncomingVTXOConfig describes how to materialize an Ark tx output into a
// spendable local VTXO descriptor.
//
// This is intentionally explicit: mapping Ark outputs to local keys requires
// wallet/application context that the FSM does not have.
type IncomingVTXOConfig struct {
	// OutputIndex is the Ark tx output index being materialized.
	OutputIndex uint32

	// ClientKey is the recipient key descriptor that controls this VTXO.
	ClientKey keychain.KeyDescriptor

	// OperatorKey is the operator public key used by the collaborative
	// spend path.
	OperatorKey *btcec.PublicKey

	// ExitDelay is the unilateral CSV delay used by the timeout spend
	// path.
	ExitDelay uint32

	// Metadata carries authoritative lineage and expiry attributes.
	Metadata IncomingVTXOMetadata
}

// BuildIncomingVTXODescriptor constructs a VTXO descriptor for a recipient
// output of a canonical Ark PSBT.
//
// The returned VTXO descriptor uses an outpoint of (arkTxid, outputIndex) and
// derives the tapscript from (client key, operator key, exit delay). This
// allows later signing of collaborative and timeout spends.
func BuildIncomingVTXODescriptor(ark *psbt.Packet,
	cfg IncomingVTXOConfig) (*vtxo.Descriptor, error) {

	switch {
	case ark == nil || ark.UnsignedTx == nil:
		return nil, fmt.Errorf("ark psbt must be provided")

	case cfg.ClientKey.PubKey == nil:
		return nil, fmt.Errorf("client key must be provided")

	case cfg.OperatorKey == nil:
		return nil, fmt.Errorf("operator key must be provided")

	case cfg.Metadata.RoundID == "":
		return nil, fmt.Errorf("round id must be provided")
	}

	if cfg.Metadata.CommitmentTxID == (chainhash.Hash{}) {
		return nil, fmt.Errorf("commitment tx id must be provided")
	}

	err := arktx.ValidateCanonicalPSBT(ark)
	if err != nil {
		return nil, err
	}

	tx := ark.UnsignedTx
	if int(cfg.OutputIndex) >= len(tx.TxOut) {
		return nil, fmt.Errorf("output index %d out of bounds",
			cfg.OutputIndex)
	}

	out := tx.TxOut[cfg.OutputIndex]
	if arktx.IsAnchorOutput(out) {
		return nil, fmt.Errorf("output index %d is anchor output",
			cfg.OutputIndex)
	}

	tapscript, err := scripts.VTXOTapScript(
		cfg.ClientKey.PubKey, cfg.OperatorKey, cfg.ExitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("derive vtxo tapscript: %w", err)
	}

	tapKey, err := scripts.VTXOTapKey(
		cfg.ClientKey.PubKey, cfg.OperatorKey, cfg.ExitDelay,
	)
	if err != nil {
		return nil, fmt.Errorf("derive vtxo tapkey: %w", err)
	}

	expectedPkScript, err := txscript.PayToTaprootScript(tapKey)
	if err != nil {
		return nil, fmt.Errorf("derive vtxo pkscript: %w", err)
	}

	if !bytes.Equal(expectedPkScript, out.PkScript) {
		return nil, fmt.Errorf("ark output pkscript does not match " +
			"derived vtxo pkscript")
	}

	arkTxid := tx.TxHash()

	return &vtxo.Descriptor{
		Outpoint: wire.OutPoint{
			Hash:  arkTxid,
			Index: cfg.OutputIndex,
		},
		Amount:         btcutil.Amount(out.Value),
		PkScript:       out.PkScript,
		ClientKey:      cfg.ClientKey,
		OperatorKey:    cfg.OperatorKey,
		TapScript:      tapscript,
		TreePath:       cfg.Metadata.TreePath,
		RoundID:        cfg.Metadata.RoundID,
		CommitmentTxID: cfg.Metadata.CommitmentTxID,
		BatchExpiry:    cfg.Metadata.BatchExpiry,
		RelativeExpiry: cfg.ExitDelay,
		TreeDepth:      cfg.Metadata.TreeDepth,
		ChainDepth:     cfg.Metadata.ChainDepth,
		CreatedHeight:  cfg.Metadata.CreatedHeight,
		Status:         vtxo.VTXOStatusLive,
	}, nil
}
