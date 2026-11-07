package round

import (
	"bytes"
	"fmt"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightninglabs/wavelength/lib/types"
	"github.com/lightninglabs/wavelength/wallet"
	fn "github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/tlv"
)

// durableBoarding keeps wallet state separate from the submitted request.
// Both may carry different proofs or metadata while an intent is assembling.
type durableBoarding struct {
	Address            []byte
	Tapscript          []byte
	OwnerKey           []byte
	OperatorKey        []byte
	ExitDelay          uint32
	Outpoint           []byte
	ConfHeight         uint32
	ConfHash           [32]byte
	ConfTx             []byte
	ChainOutpoint      []byte
	Amount             uint64
	ChainProof         []byte
	Status             uint8
	RequestOutpoint    []byte
	Policy             []byte
	ClientPubKey       []byte
	RequestOperatorKey []byte
	RequestExitDelay   uint32
	RequestProof       []byte
	AssetRef           []byte
	AssetAmount        uint64
	AssetDigest        []byte
	AssetProof         []byte
	AssetLeafHash      []byte
	AssetWitness       []byte
	AuthSpend          []byte
}

// records gives every local and requested field a stable TLV identity.
func (d *durableBoarding) records() []tlv.Record {
	return []tlv.Record{
		tlv.MakePrimitiveRecord(1, &d.Address),
		tlv.MakePrimitiveRecord(3, &d.Tapscript),
		tlv.MakePrimitiveRecord(5, &d.OwnerKey),
		tlv.MakePrimitiveRecord(7, &d.OperatorKey),
		tlv.MakePrimitiveRecord(9, &d.ExitDelay),
		tlv.MakePrimitiveRecord(11, &d.Outpoint),
		tlv.MakePrimitiveRecord(13, &d.ConfHeight),
		tlv.MakePrimitiveRecord(15, &d.ConfHash),
		tlv.MakePrimitiveRecord(17, &d.ConfTx),
		tlv.MakePrimitiveRecord(19, &d.ChainOutpoint),
		tlv.MakePrimitiveRecord(21, &d.Amount),
		tlv.MakePrimitiveRecord(23, &d.ChainProof),
		tlv.MakePrimitiveRecord(25, &d.Status),
		tlv.MakePrimitiveRecord(27, &d.RequestOutpoint),
		tlv.MakePrimitiveRecord(29, &d.Policy),
		tlv.MakePrimitiveRecord(31, &d.ClientPubKey),
		tlv.MakePrimitiveRecord(33, &d.RequestOperatorKey),
		tlv.MakePrimitiveRecord(35, &d.RequestExitDelay),
		tlv.MakePrimitiveRecord(37, &d.RequestProof),
		tlv.MakePrimitiveRecord(39, &d.AssetRef),
		tlv.MakePrimitiveRecord(41, &d.AssetAmount),
		tlv.MakePrimitiveRecord(43, &d.AssetDigest),
		tlv.MakePrimitiveRecord(45, &d.AssetProof),
		tlv.MakePrimitiveRecord(47, &d.AssetLeafHash),
		tlv.MakePrimitiveRecord(49, &d.AssetWitness),
		tlv.MakePrimitiveRecord(51, &d.AuthSpend),
	}
}

// encodeDurableBoarding records the existing intent without deriving keys or
// rebuilding a proof against a chain that may have changed since admission.
func encodeDurableBoarding(intent BoardingIntent) ([]byte, error) {
	if intent.Request.AssetRef != "" && intent.AuthSpend == nil {
		return nil, fmt.Errorf("asset boarding requires " +
			"authorization path")
	}
	d := durableBoarding{
		OwnerKey: durableKeyDescriptor(
			intent.Address.KeyDesc,
		),
		OperatorKey:        durablePubKey(intent.Address.OperatorKey),
		ExitDelay:          intent.Address.ExitDelay,
		Outpoint:           durableOutpoint(intent.Outpoint),
		ConfHeight:         uint32(intent.ChainInfo.ConfHeight),
		ConfHash:           intent.ChainInfo.ConfHash,
		ChainOutpoint:      durableOutpoint(intent.ChainInfo.OutPoint),
		Amount:             uint64(intent.ChainInfo.Amount),
		Status:             uint8(intent.Status),
		Policy:             intent.Request.PolicyTemplate,
		ClientPubKey:       durablePubKey(intent.Request.ClientKey),
		RequestOperatorKey: durablePubKey(intent.Request.OperatorKey),
		RequestExitDelay:   intent.Request.ExitDelay,
		AssetRef:           []byte(intent.Request.AssetRef),
		AssetAmount:        intent.Request.AssetAmount,
		AssetDigest:        intent.Request.AssetDigest,
		AssetProof:         intent.Request.AssetProof,
		AssetLeafHash:      intent.Request.AssetCommitmentLeafHash,
	}
	if intent.Address.Address != nil {
		d.Address = []byte(intent.Address.Address.EncodeAddress())
	}
	if intent.Request.Outpoint != nil {
		d.RequestOutpoint = durableOutpoint(*intent.Request.Outpoint)
	}
	var err error
	if intent.AuthSpend != nil {
		d.AuthSpend, err = intent.AuthSpend.Encode()
		if err != nil {
			return nil, err
		}
	}
	d.Tapscript, err = encodeDurableTapscript(intent.Address.Tapscript)
	if err != nil {
		return nil, err
	}
	if intent.ChainInfo.ConfTx != nil {
		var tx bytes.Buffer
		if err := intent.ChainInfo.ConfTx.Serialize(&tx); err != nil {
			return nil, err
		}
		d.ConfTx = tx.Bytes()
	}
	d.ChainProof, err = encodeDurableProof(intent.ChainInfo.TxProof)
	if err != nil {
		return nil, err
	}
	d.RequestProof, err = encodeDurableProof(intent.Request.TxProof)
	if err != nil {
		return nil, err
	}
	d.AssetWitness, err = encodeDurableList(
		intent.Request.AssetWitness,
		func(item []byte) ([]byte, error) { return item, nil },
	)
	if err != nil {
		return nil, err
	}

	return encodeDurableFields(d.records()...)
}

// decodeDurableBoarding restores local ownership and the submitted request
// independently. The network is supplied by the actor's fixed configuration.
func decodeDurableBoarding(raw []byte,
	params *chaincfg.Params) (BoardingIntent, error) {

	var intent BoardingIntent
	var d durableBoarding
	var err error
	if err = decodeDurableFields(raw, d.records()...); err != nil {
		return intent, err
	}
	if len(d.AssetRef) != 0 && len(d.AuthSpend) == 0 {
		return intent, fmt.Errorf("asset boarding checkpoint missing " +
			"authorization path")
	}
	if len(d.AuthSpend) != 0 {
		intent.AuthSpend, err = arkscript.DecodeSpendPath(d.AuthSpend)
		if err != nil {
			return intent, err
		}
	}
	intent.Address.ExitDelay = d.ExitDelay
	intent.ChainInfo.ConfHeight = int32(d.ConfHeight)
	intent.ChainInfo.ConfHash = d.ConfHash
	intent.ChainInfo.Amount = btcutil.Amount(int64(d.Amount))
	intent.Status = wallet.BoardingStatus(d.Status)
	intent.Request.PolicyTemplate = d.Policy
	intent.Request.ExitDelay = d.RequestExitDelay
	intent.Request.AssetRef = string(d.AssetRef)
	intent.Request.AssetAmount = d.AssetAmount
	intent.Request.AssetDigest = d.AssetDigest
	intent.Request.AssetProof = d.AssetProof
	intent.Request.AssetCommitmentLeafHash = d.AssetLeafHash
	intent.Request.AssetWitness, err = decodeDurableList(
		d.AssetWitness,
		func(item []byte) ([]byte, error) { return item, nil },
	)
	if err != nil {
		return intent, err
	}
	if len(d.Address) != 0 {
		if params == nil {
			return intent, fmt.Errorf("missing boarding network")
		}
		intent.Address.Address, err = btcaddr.DecodeAddress(
			string(d.Address), params,
		)
		if err != nil {
			return intent, err
		}
	}
	intent.Address.Tapscript, err = decodeDurableTapscript(d.Tapscript)
	if err != nil {
		return intent, err
	}
	intent.Address.KeyDesc, err = parseDurableKeyDescriptor(d.OwnerKey)
	if err != nil {
		return intent, err
	}
	intent.Address.OperatorKey, err = parseDurablePubKey(d.OperatorKey)
	if err != nil {
		return intent, err
	}
	intent.Outpoint, err = parseDurableOutpoint(d.Outpoint)
	if err != nil {
		return intent, err
	}
	intent.ChainInfo.OutPoint, err = parseDurableOutpoint(d.ChainOutpoint)
	if err != nil {
		return intent, err
	}
	if len(d.ConfTx) != 0 {
		reader := bytes.NewReader(d.ConfTx)
		intent.ChainInfo.ConfTx = wire.NewMsgTx(2)
		if err := intent.ChainInfo.ConfTx.Deserialize(
			reader,
		); err != nil {
			return intent, err
		}
		if reader.Len() != 0 {
			return intent, fmt.Errorf("trailing boarding tx bytes")
		}
	}
	intent.ChainInfo.TxProof, err = decodeDurableProof(d.ChainProof)
	if err != nil {
		return intent, err
	}
	intent.Request.TxProof, err = decodeDurableProof(d.RequestProof)
	if err != nil {
		return intent, err
	}
	if len(d.RequestOutpoint) != 0 {
		point, err := parseDurableOutpoint(d.RequestOutpoint)
		if err != nil {
			return intent, err
		}
		intent.Request.Outpoint = &point
	}
	intent.Request.ClientKey, err = parseDurablePubKey(d.ClientPubKey)
	if err != nil {
		return intent, err
	}
	intent.Request.OperatorKey, err = parseDurablePubKey(
		d.RequestOperatorKey,
	)
	if err != nil {
		return intent, err
	}

	return intent, nil
}

// encodeDurableProof retains absence rather than fabricating an empty proof.
func encodeDurableProof(value fn.Option[types.TxProof]) ([]byte, error) {
	if value.IsNone() {
		return nil, nil
	}
	p := value.UnwrapOr(types.TxProof{})

	return types.SerializeTxProof(&p)
}

// decodeDurableProof restores the optional proof used by normal validation.
func decodeDurableProof(raw []byte) (fn.Option[types.TxProof], error) {
	p, err := types.DeserializeTxProof(raw)
	if err != nil {
		return fn.None[types.TxProof](), err
	}
	if p == nil {
		return fn.None[types.TxProof](), nil
	}

	return fn.Some(*p), nil
}
