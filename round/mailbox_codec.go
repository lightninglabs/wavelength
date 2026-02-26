package round

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/darepo-client/lib/tree"
	"github.com/lightninglabs/darepo-client/roundwire"
)

// DecodeServerMailboxPayload decodes a roundwire payload into a client round
// FSM event.
func DecodeServerMailboxPayload(
	method string, raw []byte,
) (ClientEvent, error) {

	switch method {
	case roundwire.MethodClientSuccessResp:
		return decodeClientSuccessResp(raw)

	case roundwire.MethodClientBatchInfo:
		return decodeClientBatchInfo(raw)

	case roundwire.MethodClientAwaitingInputSigsResp:
		return decodeClientAwaitingInputSigsResp(raw)

	case roundwire.MethodClientVTXOAggNonces:
		return decodeClientVTXOAggNonces(raw)

	case roundwire.MethodClientVTXOAggSigs:
		return decodeClientVTXOAggSigs(raw)

	case roundwire.MethodClientErrorResp:
		return decodeClientErrorResp(raw)

	case roundwire.MethodClientRoundFailedResp:
		return decodeClientRoundFailedResp(raw)

	default:
		return nil, fmt.Errorf("unknown roundwire method: %s", method)
	}
}

func decodeClientSuccessResp(raw []byte) (ClientEvent, error) {
	var payload roundwire.ClientSuccessRespPayload
	if err := roundwire.DecodePayload(raw, &payload); err != nil {
		return nil, err
	}

	roundID, err := ParseRoundID(payload.RoundId)
	if err != nil {
		return nil, err
	}

	boardingOutpoints, err := decodeOutpointSlice(
		payload.AcceptedBoardingOutpoints,
	)
	if err != nil {
		return nil, err
	}

	vtxoOutpoints, err := decodeOutpointSlice(
		payload.AcceptedVtxoOutpoints,
	)
	if err != nil {
		return nil, err
	}

	return &RoundJoined{
		RoundID:                   roundID,
		AcceptedBoardingOutpoints: boardingOutpoints,
		AcceptedVTXOOutpoints:     vtxoOutpoints,
	}, nil
}

func decodeClientBatchInfo(raw []byte) (ClientEvent, error) {
	var payload roundwire.ClientBatchInfoPayload
	if err := roundwire.DecodePayload(raw, &payload); err != nil {
		return nil, err
	}

	roundID, err := ParseRoundID(payload.RoundId)
	if err != nil {
		return nil, err
	}

	packet, err := roundwire.DecodePSBT(payload.BatchPsbtHex)
	if err != nil {
		return nil, err
	}

	treePaths := make(map[int]*tree.Tree, len(payload.VtxoTreePaths))
	for _, path := range payload.VtxoTreePaths {
		if path == nil {
			continue
		}

		decodedTree, decodeErr := roundwire.DecodeTree(path.Tree)
		if decodeErr != nil {
			return nil, decodeErr
		}

		treePaths[int(path.OutputIndex)] = decodedTree
	}

	forfeitMappings := make(
		map[wire.OutPoint]*ConnectorLeafInfo,
		len(payload.ConnectorLeaves),
	)
	for _, leaf := range payload.ConnectorLeaves {
		if leaf == nil || leaf.VtxoOutpoint == nil ||
			leaf.LeafOutpoint == nil || leaf.LeafOutput == nil {

			return nil, fmt.Errorf(
				"connector leaf payload incomplete",
			)
		}

		vtxoOutpoint, decodeErr := roundwire.DecodeOutPoint(
			leaf.VtxoOutpoint,
		)
		if decodeErr != nil {
			return nil, decodeErr
		}

		connectorOutpoint, decodeErr := roundwire.DecodeOutPoint(
			leaf.LeafOutpoint,
		)
		if decodeErr != nil {
			return nil, decodeErr
		}

		leafOutput, decodeErr := roundwire.DecodeTxOut(leaf.LeafOutput)
		if decodeErr != nil {
			return nil, decodeErr
		}

		forfeitMappings[vtxoOutpoint] = &ConnectorLeafInfo{
			ConnectorOutpoint: connectorOutpoint,
			ConnectorPkScript: leafOutput.PkScript,
			ConnectorAmount:   leafOutput.Value,
		}
	}

	return &CommitmentTxBuilt{
		RoundID:         roundID,
		Tx:              packet,
		VTXOTreePaths:   treePaths,
		ForfeitMappings: forfeitMappings,
	}, nil
}

func decodeClientAwaitingInputSigsResp(raw []byte) (ClientEvent, error) {
	var payload roundwire.ClientAwaitingInputSigsRespPayload
	if err := roundwire.DecodePayload(raw, &payload); err != nil {
		return nil, err
	}

	roundID, err := ParseRoundID(payload.RoundId)
	if err != nil {
		return nil, err
	}

	return &AwaitingBoardingSigs{
		RoundID: roundID,
	}, nil
}

func decodeClientVTXOAggNonces(raw []byte) (ClientEvent, error) {
	var payload roundwire.ClientVTXOAggNoncesPayload
	if err := roundwire.DecodePayload(raw, &payload); err != nil {
		return nil, err
	}

	roundID, err := ParseRoundID(payload.RoundId)
	if err != nil {
		return nil, err
	}

	nonces := make(map[tree.TxID]tree.Musig2PubNonce, len(payload.Nonces))
	for _, nonceEntry := range payload.Nonces {
		txID, decodeErr := chainhash.NewHashFromStr(
			nonceEntry.TxIdHex,
		)
		if decodeErr != nil {
			return nil, decodeErr
		}

		nonce, decodeErr := roundwire.DecodeNonce(
			nonceEntry.NonceHex,
		)
		if decodeErr != nil {
			return nil, decodeErr
		}

		nonces[*txID] = nonce
	}

	return &NoncesAggregated{
		RoundID:   roundID,
		AggNonces: nonces,
	}, nil
}

func decodeClientVTXOAggSigs(raw []byte) (ClientEvent, error) {
	var payload roundwire.ClientVTXOAggSigsPayload
	if err := roundwire.DecodePayload(raw, &payload); err != nil {
		return nil, err
	}

	roundID, err := ParseRoundID(payload.RoundId)
	if err != nil {
		return nil, err
	}

	sigs := make(map[tree.TxID]*schnorr.Signature, len(payload.Signatures))
	for _, sigEntry := range payload.Signatures {
		txID, decodeErr := chainhash.NewHashFromStr(
			sigEntry.TxIdHex,
		)
		if decodeErr != nil {
			return nil, decodeErr
		}

		sig, decodeErr := roundwire.DecodeSchnorrSignature(
			sigEntry.SignatureHex,
		)
		if decodeErr != nil {
			return nil, decodeErr
		}

		sigs[*txID] = sig
	}

	return &OperatorSigned{
		RoundID: roundID,
		AggSigs: sigs,
	}, nil
}

func decodeClientErrorResp(raw []byte) (ClientEvent, error) {
	var payload roundwire.ClientErrorRespPayload
	if err := roundwire.DecodePayload(raw, &payload); err != nil {
		return nil, err
	}

	return &BoardingFailed{
		Reason:      payload.Error,
		Recoverable: true,
	}, nil
}

func decodeClientRoundFailedResp(raw []byte) (ClientEvent, error) {
	var payload roundwire.ClientRoundFailedRespPayload
	if err := roundwire.DecodePayload(raw, &payload); err != nil {
		return nil, err
	}

	return &BoardingFailed{
		Reason:      payload.Reason,
		Recoverable: true,
	}, nil
}

func decodeOutpointSlice(
	payloads []*roundwire.OutPointPayload,
) ([]wire.OutPoint, error) {

	outpoints := make([]wire.OutPoint, 0, len(payloads))
	for _, payload := range payloads {
		if payload == nil {
			continue
		}

		decoded, err := roundwire.DecodeOutPoint(payload)
		if err != nil {
			return nil, err
		}

		outpoints = append(outpoints, decoded)
	}

	return outpoints, nil
}
