package waved

import (
	"bytes"
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	oortx "github.com/lightninglabs/wavelength/lib/tx/oor"
	"github.com/lightninglabs/wavelength/lib/tx/psbtutil"
	"github.com/lightninglabs/wavelength/oor"
	"github.com/lightninglabs/wavelength/vtxo"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// checkCooperativeFunding binds an explicit funding descriptor to the wallet's
// record when known. An external VTXO's funding data comes from the authorized
// caller's swap record, not from the PSBT offered for signature.
func checkCooperativeFunding(ci *waverpc.CustomOORInput,
	stored *vtxo.Descriptor, operator *btcec.PublicKey) error {

	if ci.AmountSat <= 0 || len(ci.PkScript) == 0 ||
		len(ci.VtxoPolicyTemplate) == 0 || len(ci.SpendPath) == 0 {
		return fmt.Errorf("complete cooperative funding descriptor " +
			"required")
	}
	if stored == nil {
		return nil
	}
	matchesPolicy := len(stored.PolicyTemplate) == 0 ||
		bytes.Equal(stored.PolicyTemplate, ci.VtxoPolicyTemplate)
	matchesOperator := stored.OperatorKey == nil ||
		sameXOnlyPubKey(stored.OperatorKey, operator)
	if int64(stored.Amount) != ci.AmountSat ||
		!bytes.Equal(stored.PkScript, ci.PkScript) ||
		!matchesPolicy || !matchesOperator {
		return fmt.Errorf("cooperative input differs from stored " +
			"funding")
	}

	return nil
}

// checkCooperativeIntent admits only full-value, single-recipient spends in
// shared-owner mode. Prepare and cosign take unsigned inputs; Send needs both
// independent authorizations before it may dispatch a transfer.
func checkCooperativeIntent(inputs []*waverpc.CustomOORInput,
	outputs []*waverpc.Output, send bool) (bool, error) {

	for _, in := range inputs {
		if !in.GetPreserveSourceOwners() {
			if len(in.GetPayoutSignatures()) != 0 {
				return false, fmt.Errorf("unexpected payout " +
					"signatures")
			}

			continue
		}
		if len(inputs) != 1 || len(outputs) != 1 ||
			in.AmountSat <= 0 ||
			in.AmountSat != outputs[0].GetAmountSat() {
			return false, fmt.Errorf("cooperative spend requires " +
				"one input and one full-value payout")
		}
		want := 0
		if send {
			want = 1
		}
		if len(in.ExternalSignatures) != want ||
			len(in.PayoutSignatures) != want {
			return false, fmt.Errorf("expected %d checkpoint and "+
				"payout signatures", want)
		}

		return true, nil
	}

	return false, nil
}

// cooperativeOwnerLeaf retains the funded leaf verbatim instead of deriving a
// local-owner-only checkpoint. A constrained/unilateral leaf is not a generic
// cooperative authorization.
func cooperativeOwnerLeaf(template *arkscript.PolicyTemplate,
	local, operator *btcec.PublicKey, rawPath []byte) ([]byte, []byte,
	error) {

	tree, err := template.Compile()
	if err != nil {
		return nil, nil, err
	}
	for _, leaf := range template.Leaves {
		multi, ok := leaf.Node.(*arkscript.Multisig)
		if !ok || len(multi.Keys) != 3 ||
			!arkscript.ContainsKey(multi, local) ||
			!arkscript.ContainsKey(multi, operator) ||
			sameXOnlyPubKey(local, operator) {

			continue
		}
		seen := make(map[string]bool)
		for _, key := range multi.Keys {
			seen[string(schnorr.SerializePubKey(key))] = true
		}
		if len(seen) != len(multi.Keys) {
			continue
		}
		path, err := tree.SpendPathForNode(multi, nil)
		if err != nil {
			return nil, nil, err
		}
		encoded, err := path.Encode()
		if err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(rawPath, encoded) {
			continue
		}
		policy, err := leaf.Encode()

		return path.WitnessScript, policy, err
	}

	return nil, nil, fmt.Errorf("no matching canonical funded co-owner " +
		"leaf")
}

// verifyCooperativeSubmit checks both authorizations on the same package the
// actor will build. The operator is not a substitute for the other owner.
func verifyCooperativeSubmit(policy arkscript.CheckpointPolicy,
	inputs []oor.TransferInput, recipients []oortx.RecipientOutput) error {

	ark, cps, err := oor.BuildSubmitPackage(policy, inputs, recipients)
	if err != nil {
		return err
	}
	in := &inputs[0]
	for _, key := range in.CustomSpendKeys {
		if sameXOnlyPubKey(key, in.VTXO.ClientKey.PubKey) ||
			sameXOnlyPubKey(key, policy.OperatorKey) {

			continue
		}
		err := oor.VerifySingleInputAuthorization(
			cps[0], key, in.CustomSpend.WitnessScript,
			in.ExternalSignatures[0],
		)
		if err != nil {
			return fmt.Errorf("checkpoint authorization: %w", err)
		}

		return oor.VerifySingleInputAuthorization(
			ark, key, in.OwnerLeafScript,
			in.ExternalArkSignatures[0],
		)
	}

	return fmt.Errorf("missing cooperative source co-owner")
}

// signCooperativePackage signs only the canonical package rebuilt from the
// authenticated caller's funded outpoint, policy, amount and payout. Comparing
// serialized packets also binds witness UTXOs, leaf scripts and control data.
func (r *RPCServer) signCooperativePackage(ctx context.Context,
	pkg *waverpc.CooperativeOORPackage) (
	*waverpc.SignOORCustomInputResponse, error) {

	inputs := []*waverpc.CustomOORInput{pkg.CustomInput}
	outputs := []*waverpc.Output{pkg.Recipient}
	ok, err := checkCooperativeIntent(inputs, outputs, false)
	if err != nil || !ok || len(pkg.CheckpointPsbt) == 0 ||
		len(pkg.ArkPsbt) == 0 {
		return nil, status.Error(
			codes.InvalidArgument,
			"incomplete cooperative signing intent",
		)
	}
	if r.server.vtxoStore == nil {
		return nil, status.Error(
			codes.Internal, "VTXO store not initialized",
		)
	}
	terms, err := r.server.fetchOperatorTerms(ctx)
	if err != nil {
		return nil, operatorTermsError(
			"unable to fetch operator terms", err,
		)
	}
	transfer, err := BuildCustomTransferInputs(
		ctx, r.server.vtxoStore, inputs, r.server.loadClientKeyDesc(),
		terms.PubKey, terms.VTXOExitDelay,
	)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	recipients, err := r.buildSendOORRecipients(ctx, outputs, terms)
	if err != nil {
		return nil, err
	}
	ark, cps, err := oor.BuildSubmitPackage(arkscript.CheckpointPolicy{
		OperatorKey: terms.PubKey, CSVDelay: terms.VTXOExitDelay,
	}, transfer, recipients)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	for _, packet := range []struct {
		expected *psbt.Packet
		received []byte
	}{
		{
			ark,
			pkg.ArkPsbt,
		}, {
			cps[0],
			pkg.CheckpointPsbt,
		},
	} {
		raw, err := psbtutil.Serialize(packet.expected)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if !bytes.Equal(raw, packet.received) {
			return nil, status.Error(
				codes.InvalidArgument, "unsigned package "+
					"does not match funded input and "+
					"payout",
			)
		}
	}
	signer, err := r.oorSigner()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	cpSig, err := oor.SignCustomCheckpointInput(
		signer, &transfer[0], cps[0],
	)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := oor.SignArkPSBT(signer, ark, cps, transfer); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	payout := ark.Inputs[0].TaprootScriptSpendSig
	if len(payout) != 1 {
		return nil, status.Error(
			codes.Internal, "expected one wallet payout signature",
		)
	}
	local := transfer[0].VTXO.ClientKey.PubKey
	arkSig := &oor.ExternalTaprootScriptSignature{
		PubKey: local, WitnessScript: transfer[0].OwnerLeafScript,
		Signature: payout[0].Signature, SigHash: payout[0].SigHash,
	}
	// A signer backend failure must never produce a partial authorization.
	if err := oor.VerifySingleInputAuthorization(
		cps[0], local, transfer[0].CustomSpend.WitnessScript, *cpSig,
	); err != nil {
		return nil, status.Errorf(codes.Internal, "wallet checkpoint "+
			"signature: %v", err)
	}
	if err := oor.VerifySingleInputAuthorization(
		ark, local, transfer[0].OwnerLeafScript, *arkSig,
	); err != nil {
		return nil, status.Errorf(codes.Internal, "wallet payout "+
			"signature: %v", err)
	}

	return &waverpc.SignOORCustomInputResponse{
		Signature:       rpcScriptSignature(cpSig),
		PayoutSignature: rpcScriptSignature(arkSig),
	}, nil
}

// rpcScriptSignature converts a wallet-generated signature to its wire record.
func rpcScriptSignature(
	s *oor.ExternalTaprootScriptSignature) *waverpc.TaprootScriptSignature {

	return &waverpc.TaprootScriptSignature{
		Pubkey:        s.PubKey.SerializeCompressed(),
		WitnessScript: s.WitnessScript,
		Signature:     s.Signature,
		Sighash:       uint32(s.SigHash),
	}
}
