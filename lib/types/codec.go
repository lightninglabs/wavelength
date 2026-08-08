package types

import (
	"bytes"
	"fmt"
	"io"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/lib/arkscript"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// joinRoundAuthMessageVersion is the canonical message encoding version
	// used for join-round authentication payloads.
	joinRoundAuthMessageVersion uint64 = 3

	// joinRoundAuthDomainTag domain-separates join request authentication
	// payloads from other signed messages.
	joinRoundAuthDomainTag = "wavelength-join-round-auth"
)

const (
	joinRoundAuthMessageVersionRecordType tlv.Type = 1
	joinRoundAuthMessageDomainRecordType  tlv.Type = 2
	joinRoundAuthMessageIDRecordType      tlv.Type = 3
	joinRoundAuthMessageBoardRecordType   tlv.Type = 4
	joinRoundAuthMessageVTXORecordType    tlv.Type = 5
	joinRoundAuthMessageForfeitRecordType tlv.Type = 6
	joinRoundAuthMessageLeaveRecordType   tlv.Type = 7
)

const (
	joinRoundAuthOutPointHashRecordType        tlv.Type = 1
	joinRoundAuthOutPointIndexRecordType       tlv.Type = 2
	joinRoundAuthForfeitAuthSpendRecordType    tlv.Type = 3
	joinRoundAuthForfeitForfeitSpendRecordType tlv.Type = 4
)

const (
	joinRoundAuthBoardPolicyRecordType tlv.Type = 3
)

const (
	joinRoundAuthVTXOAmountRecordType      tlv.Type = 1
	joinRoundAuthVTXOPolicyRecordType      tlv.Type = 2
	joinRoundAuthVTXOSigningKeyRecordType  tlv.Type = 3
	joinRoundAuthVTXOIsChangeRecordType    tlv.Type = 4
	joinRoundAuthVTXOFixedAmountRecordType tlv.Type = 5
)

const (
	joinRoundAuthLeaveValueRecordType    tlv.Type = 1
	joinRoundAuthLeaveScriptRecordType   tlv.Type = 2
	joinRoundAuthLeaveIsChangeRecordType tlv.Type = 3
)

const (
	// joinRoundAuthMaxRequestCount is the max number of entries allowed for
	// each request list when decoding join-auth messages.
	joinRoundAuthMaxRequestCount uint64 = 1024

	// joinRoundAuthMaxBlobEntrySize is the max byte size allowed for one
	// list entry blob while decoding.
	joinRoundAuthMaxBlobEntrySize uint64 = 64 * 1024

	// joinRoundAuthMaxScriptSize is the max script size accepted while
	// decoding VTXO and leave outputs.
	joinRoundAuthMaxScriptSize = 10_000
)

// JoinRoundAuth contains the BIP-322 payload for a JoinRoundRequest.
//
// Message stores the exact canonical bytes that were signed. Signature
// stores the full-format BIP-322 payload, which is the serialized to_sign
// transaction.
type JoinRoundAuth struct {
	// Message is the canonical join message signed by the client.
	Message []byte

	// ValidFrom is the first block height where this auth intent is
	// accepted.
	ValidFrom uint32

	// ValidUntil is the last block height where this auth intent is
	// accepted. A value of 0 means no upper bound.
	ValidUntil uint32

	// Signature is the serialized full-format BIP-322 signature
	// payload.
	Signature []byte
}

// JoinRoundAuthMessage serializes a JoinRoundRequest into deterministic,
// versioned TLV bytes for BIP-322 signing and verification.
//
// The resulting bytes are the message that the client signs with BIP-322
// to prove authorization of the join request contents.
//
// Top-level envelope (TLV stream):
//
//	+-------+-------------------------------------------+
//	| type  | field                                     |
//	+-------+-------------------------------------------+
//	|   1   | version   (uint64, currently 3)           |
//	|   2   | domain    ("wavelength-join-round-auth")   |
//	|   3   | identifier (33-byte compressed pubkey)    |
//	|   4   | boarding  (blob list)                     |
//	|   5   | vtxos     (blob list)                     |
//	|   6   | forfeits  (blob list)                     |
//	|   7   | leaves    (blob list)                     |
//	+-------+-------------------------------------------+
//
// Each blob list is encoded as:
//
//	varint(count) || for each entry: varint(len) || entry_bytes
//
// Boarding entry TLV:
//
//	1: outpoint hash  |  2: outpoint index
//	3: client key     |  4: operator key   |  5: exit delay
//
// VTXO entry TLV:
//
//	1: amount      |  2: pkScript   |  3: expiry
//	4: client key  |  5: operator key  |  6: signing key
//
// Forfeit entry TLV:
//
//	1: outpoint hash  |  2: outpoint index
//	3: auth spend     |  4: forfeit spend
//
// Leave entry TLV:
//
//	1: value  |  2: pkScript
func JoinRoundAuthMessage(req *JoinRoundRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("join round request must be provided")
	}

	if req.Identifier == nil {
		return nil, fmt.Errorf("join round request identifier must " +
			"be provided")
	}

	boardingBlob, err := encodeJoinAuthBoardingRequests(req.BoardingReqs)
	if err != nil {
		return nil, err
	}

	vtxoBlob, err := encodeJoinAuthVTXORequests(req.VTXOReqs)
	if err != nil {
		return nil, err
	}

	forfeitBlob, err := encodeJoinAuthForfeitRequests(req.ForfeitReqs)
	if err != nil {
		return nil, err
	}

	leaveBlob, err := encodeJoinAuthLeaveRequests(req.LeaveReqs)
	if err != nil {
		return nil, err
	}

	version := joinRoundAuthMessageVersion
	domainTag := []byte(joinRoundAuthDomainTag)
	identifier := req.Identifier.SerializeCompressed()

	records := make([]tlv.Record, 0, 7)
	records = append(
		records, tlv.MakePrimitiveRecord(
			joinRoundAuthMessageVersionRecordType, &version,
		),
	)
	records = append(
		records, tlv.MakePrimitiveRecord(
			joinRoundAuthMessageDomainRecordType, &domainTag,
		),
	)
	records = append(
		records, tlv.MakePrimitiveRecord(
			joinRoundAuthMessageIDRecordType, &identifier,
		),
	)

	records = append(
		records, tlv.MakePrimitiveRecord(
			joinRoundAuthMessageBoardRecordType, &boardingBlob,
		),
	)
	records = append(
		records, tlv.MakePrimitiveRecord(
			joinRoundAuthMessageVTXORecordType, &vtxoBlob,
		),
	)
	records = append(
		records, tlv.MakePrimitiveRecord(
			joinRoundAuthMessageForfeitRecordType, &forfeitBlob,
		),
	)
	records = append(
		records, tlv.MakePrimitiveRecord(
			joinRoundAuthMessageLeaveRecordType, &leaveBlob,
		),
	)

	return encodeJoinAuthTLV(records)
}

// DecodeJoinRoundAuthMessage parses canonical join-auth message bytes back
// into a JoinRoundRequest while enforcing decode-time size limits.
func DecodeJoinRoundAuthMessage(raw []byte) (*JoinRoundRequest, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("join auth message bytes must be " +
			"provided")
	}

	var (
		version     uint64
		domain      []byte
		identifier  []byte
		boardingRaw []byte
		vtxoRaw     []byte
		forfeitRaw  []byte
		leaveRaw    []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageVersionRecordType, &version,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageDomainRecordType, &domain,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageIDRecordType, &identifier,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageBoardRecordType, &boardingRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageVTXORecordType, &vtxoRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageForfeitRecordType, &forfeitRaw,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthMessageLeaveRecordType, &leaveRaw,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create join auth decode stream: %w",
			err)
	}

	// Pre-validate the framing so a record declaring a length larger than
	// the bytes present cannot drive an unbounded make() inside the tlv
	// decoder. These bytes are the attacker-controlled, BIP-322-signed join
	// intent crossing the client/server trust boundary.
	reader, err := safeTypesTLVBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("decode join auth message envelope: %w",
			err)
	}

	parsedTypes, err := stream.DecodeWithParsedTypes(reader)
	if err != nil {
		return nil, fmt.Errorf("decode join auth message envelope: %w",
			err)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("decode join auth message envelope: %d "+
			"trailing bytes", reader.Len())
	}

	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthMessageVersionRecordType, "version",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthMessageDomainRecordType, "domain",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthMessageIDRecordType, "identifier",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthMessageBoardRecordType, "boarding",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthMessageVTXORecordType, "vtxo",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthMessageForfeitRecordType, "forfeit",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthMessageLeaveRecordType, "leave",
	)
	if err != nil {
		return nil, err
	}

	if version != joinRoundAuthMessageVersion {
		return nil, fmt.Errorf("unsupported join auth message "+
			"version %d", version)
	}

	if string(domain) != joinRoundAuthDomainTag {
		return nil, fmt.Errorf("unexpected join auth domain %q", domain)
	}

	boardingReqs, err := decodeJoinAuthBoardingRequests(boardingRaw)
	if err != nil {
		return nil, err
	}

	vtxoReqs, err := decodeJoinAuthVTXORequests(vtxoRaw)
	if err != nil {
		return nil, err
	}

	forfeitReqs, err := decodeJoinAuthForfeitRequests(forfeitRaw)
	if err != nil {
		return nil, err
	}

	leaveReqs, err := decodeJoinAuthLeaveRequests(leaveRaw)
	if err != nil {
		return nil, err
	}

	identifierKey, err := decodeJoinAuthPubKey(identifier)
	if err != nil {
		return nil, fmt.Errorf("decode identifier: %w", err)
	}

	return &JoinRoundRequest{
		Identifier:   identifierKey,
		BoardingReqs: boardingReqs,
		VTXOReqs:     vtxoReqs,
		ForfeitReqs:  forfeitReqs,
		LeaveReqs:    leaveReqs,
	}, nil
}

// requireJoinAuthRecord checks that the given record type was parsed.
func requireJoinAuthRecord(parsedTypes tlv.TypeMap, recordType tlv.Type,
	recordName string) error {

	if _, ok := parsedTypes[recordType]; !ok {
		return fmt.Errorf("join auth %s record must be present",
			recordName)
	}

	return nil
}

// decodeJoinAuthBoardingRequests parses a boarding request list from blob-list
// bytes.
func decodeJoinAuthBoardingRequests(raw []byte) ([]*BoardingRequest, error) {
	entries, err := decodeJoinAuthBlobList(
		raw, joinRoundAuthMaxRequestCount,
	)
	if err != nil {
		return nil, fmt.Errorf("decode boarding requests: %w", err)
	}

	requests := make([]*BoardingRequest, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		req, err := decodeJoinAuthBoardingRequest(entries[i])
		if err != nil {
			return nil, fmt.Errorf("decode boarding request %d: %w",
				i, err)
		}

		requests = append(requests, req)
	}

	return requests, nil
}

// decodeJoinAuthBoardingRequest parses one boarding request entry.
func decodeJoinAuthBoardingRequest(raw []byte) (*BoardingRequest, error) {
	var (
		hash   []byte
		index  uint64
		policy []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			joinRoundAuthOutPointHashRecordType, &hash,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthOutPointIndexRecordType, &index,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthBoardPolicyRecordType, &policy,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create boarding decode stream: %w", err)
	}

	reader, err := safeTypesTLVBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("decode boarding request: %w", err)
	}

	parsedTypes, err := stream.DecodeWithParsedTypes(reader)
	if err != nil {
		return nil, fmt.Errorf("decode boarding request: %w", err)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("decode boarding request: %d "+
			"trailing bytes", reader.Len())
	}

	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthOutPointHashRecordType,
		"outpoint hash",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthOutPointIndexRecordType,
		"outpoint index",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthBoardPolicyRecordType,
		"policy template",
	)
	if err != nil {
		return nil, err
	}

	outpoint, err := decodeJoinAuthOutPoint(hash, index)
	if err != nil {
		return nil, err
	}

	req := &BoardingRequest{
		Outpoint:       outpoint,
		PolicyTemplate: bytes.Clone(policy),
	}

	_, err = req.DecodePolicyTemplate()
	if err != nil {
		return nil, fmt.Errorf("decode policy template: %w", err)
	}

	return req, nil
}

// decodeJoinAuthVTXORequests parses a VTXO request list from blob-list bytes.
func decodeJoinAuthVTXORequests(raw []byte) ([]*VTXORequest, error) {
	entries, err := decodeJoinAuthBlobList(
		raw, joinRoundAuthMaxRequestCount,
	)
	if err != nil {
		return nil, fmt.Errorf("decode vtxo requests: %w", err)
	}

	requests := make([]*VTXORequest, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		req, err := decodeJoinAuthVTXORequest(entries[i])
		if err != nil {
			return nil, fmt.Errorf("decode vtxo request %d: %w", i,
				err)
		}

		requests = append(requests, req)
	}

	return requests, nil
}

// decodeJoinAuthVTXORequest parses one VTXO request entry.
func decodeJoinAuthVTXORequest(raw []byte) (*VTXORequest, error) {
	var (
		amount     uint64
		policy     []byte
		signingKey []byte
		isChange   uint8
		fixedAmt   uint8
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			joinRoundAuthVTXOAmountRecordType, &amount,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthVTXOPolicyRecordType, &policy,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthVTXOSigningKeyRecordType, &signingKey,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthVTXOIsChangeRecordType, &isChange,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthVTXOFixedAmountRecordType, &fixedAmt,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create vtxo decode stream: %w", err)
	}

	reader, err := safeTypesTLVBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("decode vtxo request: %w", err)
	}

	parsedTypes, err := stream.DecodeWithParsedTypes(reader)
	if err != nil {
		return nil, fmt.Errorf("decode vtxo request: %w", err)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("decode vtxo request: %d trailing bytes",
			reader.Len())
	}

	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthVTXOAmountRecordType, "vtxo amount",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthVTXOPolicyRecordType, "vtxo policy",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthVTXOSigningKeyRecordType,
		"vtxo signing key",
	)
	if err != nil {
		return nil, err
	}

	if amount > math.MaxInt64 {
		return nil, fmt.Errorf("vtxo amount %d exceeds int64", amount)
	}

	if len(policy) > joinRoundAuthMaxScriptSize {
		return nil, fmt.Errorf("vtxo policy size %d exceeds max %d",
			len(policy), joinRoundAuthMaxScriptSize)
	}

	signingPubKey, err := decodeJoinAuthPubKey(signingKey)
	if err != nil {
		return nil, fmt.Errorf("decode vtxo signing key: %w", err)
	}

	req := &VTXORequest{
		Amount:         btcutil.Amount(amount),
		IsChange:       isChange != 0,
		FixedAmount:    fixedAmt != 0,
		PolicyTemplate: bytes.Clone(policy),
		SigningKey: keychain.KeyDescriptor{
			PubKey: signingPubKey,
		},
	}

	_, err = req.DecodePolicyTemplate()
	if err != nil {
		return nil, fmt.Errorf("decode vtxo policy: %w", err)
	}

	return req, nil
}

// decodeJoinAuthForfeitRequests parses a forfeit request list from blob-list
// bytes.
func decodeJoinAuthForfeitRequests(raw []byte) ([]*ForfeitRequest, error) {
	entries, err := decodeJoinAuthBlobList(
		raw, joinRoundAuthMaxRequestCount,
	)
	if err != nil {
		return nil, fmt.Errorf("decode forfeit requests: %w", err)
	}

	requests := make([]*ForfeitRequest, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		req, err := decodeJoinAuthForfeitRequest(entries[i])
		if err != nil {
			return nil, fmt.Errorf("decode forfeit request %d: %w",
				i, err)
		}

		requests = append(requests, req)
	}

	return requests, nil
}

// decodeJoinAuthForfeitRequest parses one forfeit request entry.
func decodeJoinAuthForfeitRequest(raw []byte) (*ForfeitRequest, error) {
	var (
		hash         []byte
		index        uint64
		authSpend    []byte
		forfeitSpend []byte
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			joinRoundAuthOutPointHashRecordType, &hash,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthOutPointIndexRecordType, &index,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthForfeitAuthSpendRecordType, &authSpend,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthForfeitForfeitSpendRecordType,
			&forfeitSpend,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create forfeit decode stream: %w", err)
	}

	reader, err := safeTypesTLVBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("decode forfeit request: %w", err)
	}

	parsedTypes, err := stream.DecodeWithParsedTypes(reader)
	if err != nil {
		return nil, fmt.Errorf("decode forfeit request: %w", err)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("decode forfeit request: %d "+
			"trailing bytes", reader.Len())
	}

	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthOutPointHashRecordType,
		"outpoint hash",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthOutPointIndexRecordType,
		"outpoint index",
	)
	if err != nil {
		return nil, err
	}

	outpoint, err := decodeJoinAuthOutPoint(hash, index)
	if err != nil {
		return nil, err
	}

	authPath, err := decodeJoinAuthSpendPath("auth spend", authSpend)
	if err != nil {
		return nil, err
	}
	forfeitPath, err := decodeJoinAuthSpendPath(
		"forfeit spend", forfeitSpend,
	)
	if err != nil {
		return nil, err
	}

	return &ForfeitRequest{
		VTXOOutpoint: outpoint,
		AuthSpend:    authPath,
		ForfeitSpend: forfeitPath,
	}, nil
}

// decodeJoinAuthLeaveRequests parses a leave request list from blob-list bytes.
func decodeJoinAuthLeaveRequests(raw []byte) ([]*LeaveRequest, error) {
	entries, err := decodeJoinAuthBlobList(
		raw, joinRoundAuthMaxRequestCount,
	)
	if err != nil {
		return nil, fmt.Errorf("decode leave requests: %w", err)
	}

	requests := make([]*LeaveRequest, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		req, err := decodeJoinAuthLeaveRequest(entries[i])
		if err != nil {
			return nil, fmt.Errorf("decode leave request %d: %w", i,
				err)
		}

		requests = append(requests, req)
	}

	return requests, nil
}

// decodeJoinAuthLeaveRequest parses one leave request entry.
func decodeJoinAuthLeaveRequest(raw []byte) (*LeaveRequest, error) {
	var (
		value    uint64
		script   []byte
		isChange uint8
	)

	stream, err := tlv.NewStream(
		tlv.MakePrimitiveRecord(
			joinRoundAuthLeaveValueRecordType, &value,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthLeaveScriptRecordType, &script,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthLeaveIsChangeRecordType, &isChange,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create leave decode stream: %w", err)
	}

	reader, err := safeTypesTLVBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("decode leave request: %w", err)
	}

	parsedTypes, err := stream.DecodeWithParsedTypes(reader)
	if err != nil {
		return nil, fmt.Errorf("decode leave request: %w", err)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("decode leave request: %d "+
			"trailing bytes", reader.Len())
	}

	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthLeaveValueRecordType, "leave value",
	)
	if err != nil {
		return nil, err
	}
	err = requireJoinAuthRecord(
		parsedTypes, joinRoundAuthLeaveScriptRecordType, "leave script",
	)
	if err != nil {
		return nil, err
	}

	if value > math.MaxInt64 {
		return nil, fmt.Errorf("leave value %d exceeds int64", value)
	}

	if len(script) > joinRoundAuthMaxScriptSize {
		return nil, fmt.Errorf("leave script size %d exceeds max %d",
			len(script), joinRoundAuthMaxScriptSize)
	}

	return &LeaveRequest{
		IsChange: isChange != 0,
		Output: &wire.TxOut{
			Value:    int64(value),
			PkScript: bytes.Clone(script),
		},
	}, nil
}

// decodeJoinAuthOutPoint parses a hash/index pair into a wire outpoint.
func decodeJoinAuthOutPoint(hash []byte, index uint64) (*wire.OutPoint, error) {
	if len(hash) != chainhash.HashSize {
		return nil, fmt.Errorf("outpoint hash must be %d bytes",
			chainhash.HashSize)
	}

	if index > math.MaxUint32 {
		return nil, fmt.Errorf("outpoint index %d exceeds uint32",
			index)
	}

	var outpointHash chainhash.Hash
	copy(outpointHash[:], hash)

	return &wire.OutPoint{
		Hash:  outpointHash,
		Index: uint32(index),
	}, nil
}

// decodeJoinAuthPubKey parses a required compressed secp256k1 public key.
func decodeJoinAuthPubKey(raw []byte) (*btcec.PublicKey, error) {
	if len(raw) != btcec.PubKeyBytesLenCompressed {
		return nil, fmt.Errorf("compressed public key must be %d bytes",
			btcec.PubKeyBytesLenCompressed)
	}

	pubKey, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse compressed public key: %w", err)
	}

	return pubKey, nil
}

// decodeJoinAuthBlobList parses a varint-prefixed blob list while enforcing
// request and entry size limits.
func decodeJoinAuthBlobList(raw []byte, maxEntries uint64) ([][]byte, error) {
	var scratch [8]byte
	reader := bytes.NewReader(raw)

	count, err := tlv.ReadVarInt(reader, &scratch)
	if err != nil {
		return nil, fmt.Errorf("decode blob list count: %w", err)
	}

	if count > maxEntries {
		return nil, fmt.Errorf("blob list count %d exceeds max %d",
			count, maxEntries)
	}

	blobs := make([][]byte, 0, int(count))
	for i := uint64(0); i < count; i++ {
		size, err := tlv.ReadVarInt(reader, &scratch)
		if err != nil {
			return nil, fmt.Errorf("decode blob %d size: %w", i,
				err)
		}

		if size > joinRoundAuthMaxBlobEntrySize {
			return nil, fmt.Errorf("blob %d size %d exceeds max %d",
				i, size, joinRoundAuthMaxBlobEntrySize)
		}

		blob := make([]byte, size)
		_, err = io.ReadFull(reader, blob)
		if err != nil {
			return nil, fmt.Errorf("decode blob %d: %w", i, err)
		}

		blobs = append(blobs, blob)
	}

	if reader.Len() != 0 {
		return nil, fmt.Errorf("decode blob list: %d trailing bytes",
			reader.Len())
	}

	return blobs, nil
}

// encodeJoinAuthBoardingRequests serializes boarding request data in
// request order.
func encodeJoinAuthBoardingRequests(requests []*BoardingRequest) ([]byte,
	error) {

	entries := make([][]byte, 0, len(requests))
	for i := 0; i < len(requests); i++ {
		req := requests[i]
		if req == nil {
			return nil, fmt.Errorf("boarding request %d must be "+
				"provided", i)
		}

		entry, err := encodeJoinAuthBoardingRequest(req)
		if err != nil {
			return nil, fmt.Errorf("boarding request %d: %w", i,
				err)
		}

		entries = append(entries, entry)
	}

	return encodeJoinAuthBlobList(entries)
}

// encodeJoinAuthBoardingRequest serializes one boarding request as TLV.
func encodeJoinAuthBoardingRequest(req *BoardingRequest) ([]byte, error) {
	hash, index, err := encodeJoinAuthOutPoint(req.Outpoint)
	if err != nil {
		return nil, fmt.Errorf("outpoint: %w", err)
	}

	policyTemplate, err := req.EffectivePolicyTemplate()
	if err != nil {
		return nil, fmt.Errorf("policy template: %w", err)
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			joinRoundAuthOutPointHashRecordType, &hash,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthOutPointIndexRecordType, &index,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthBoardPolicyRecordType, &policyTemplate,
		),
	}

	return encodeJoinAuthTLV(records)
}

// encodeJoinAuthVTXORequests serializes requested VTXO outputs in request
// order.
func encodeJoinAuthVTXORequests(requests []*VTXORequest) ([]byte, error) {
	entries := make([][]byte, 0, len(requests))
	for i := 0; i < len(requests); i++ {
		req := requests[i]
		if req == nil {
			return nil, fmt.Errorf("vtxo request %d must be "+
				"provided", i)
		}

		entry, err := encodeJoinAuthVTXORequest(req)
		if err != nil {
			return nil, fmt.Errorf("vtxo request %d: %w", i, err)
		}

		entries = append(entries, entry)
	}

	return encodeJoinAuthBlobList(entries)
}

// encodeJoinAuthVTXORequest serializes one VTXO request as TLV.
func encodeJoinAuthVTXORequest(req *VTXORequest) ([]byte, error) {
	if req.Amount < 0 {
		return nil, fmt.Errorf("amount must be non-negative")
	}

	amount := uint64(req.Amount)
	policyTemplate, err := req.EffectivePolicyTemplate()
	if err != nil {
		return nil, fmt.Errorf("policy template: %w", err)
	}

	signingKey, err := encodeJoinAuthPubKey(req.SigningKey.PubKey)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}

	isChange := uint8(0)
	if req.IsChange {
		isChange = 1
	}
	fixedAmt := uint8(0)
	if req.FixedAmount {
		fixedAmt = 1
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			joinRoundAuthVTXOAmountRecordType, &amount,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthVTXOPolicyRecordType, &policyTemplate,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthVTXOSigningKeyRecordType, &signingKey,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthVTXOIsChangeRecordType, &isChange,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthVTXOFixedAmountRecordType, &fixedAmt,
		),
	}

	return encodeJoinAuthTLV(records)
}

// encodeJoinAuthForfeitRequests serializes requested forfeit outpoints in
// request order.
func encodeJoinAuthForfeitRequests(requests []*ForfeitRequest) ([]byte, error) {
	entries := make([][]byte, 0, len(requests))
	for i := 0; i < len(requests); i++ {
		req := requests[i]
		if req == nil {
			return nil, fmt.Errorf("forfeit request %d must be "+
				"provided", i)
		}

		entry, err := encodeJoinAuthForfeitRequest(req)
		if err != nil {
			return nil, fmt.Errorf("forfeit request %d: %w", i, err)
		}

		entries = append(entries, entry)
	}

	return encodeJoinAuthBlobList(entries)
}

// encodeJoinAuthForfeitRequest serializes one forfeit request as TLV.
func encodeJoinAuthForfeitRequest(req *ForfeitRequest) ([]byte, error) {
	hash, index, err := encodeJoinAuthOutPoint(req.VTXOOutpoint)
	if err != nil {
		return nil, fmt.Errorf("outpoint: %w", err)
	}

	records := make([]tlv.Record, 0, 4)
	records = append(
		records, tlv.MakePrimitiveRecord(
			joinRoundAuthOutPointHashRecordType, &hash,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthOutPointIndexRecordType, &index,
		),
	)

	if req.AuthSpend != nil {
		authSpend, err := encodeJoinAuthSpendPath(req.AuthSpend)
		if err != nil {
			return nil, fmt.Errorf("auth spend: %w", err)
		}

		records = append(
			records, tlv.MakePrimitiveRecord(
				joinRoundAuthForfeitAuthSpendRecordType,
				&authSpend,
			),
		)
	}
	if req.ForfeitSpend != nil {
		forfeitSpend, err := encodeJoinAuthSpendPath(req.ForfeitSpend)
		if err != nil {
			return nil, fmt.Errorf("forfeit spend: %w", err)
		}

		records = append(
			records, tlv.MakePrimitiveRecord(
				joinRoundAuthForfeitForfeitSpendRecordType,
				&forfeitSpend,
			),
		)
	}

	return encodeJoinAuthTLV(records)
}

// encodeJoinAuthSpendPath serializes an optional custom spend path for
// join-auth binding.
func encodeJoinAuthSpendPath(path *arkscript.SpendPath) ([]byte, error) {
	encoded, err := path.Encode()
	if err != nil {
		return nil, err
	}

	return encoded, nil
}

// decodeJoinAuthSpendPath parses an optional custom spend path from a
// join-auth forfeit entry.
func decodeJoinAuthSpendPath(label string,
	raw []byte) (*arkscript.SpendPath, error) {

	if len(raw) == 0 {
		return nil, nil
	}

	path, err := arkscript.DecodeSpendPath(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", label, err)
	}

	return path, nil
}

// encodeJoinAuthLeaveRequests serializes leave outputs in request order.
func encodeJoinAuthLeaveRequests(requests []*LeaveRequest) ([]byte, error) {
	entries := make([][]byte, 0, len(requests))
	for i := 0; i < len(requests); i++ {
		req := requests[i]
		if req == nil {
			return nil, fmt.Errorf("leave request %d must be "+
				"provided", i)
		}

		entry, err := encodeJoinAuthLeaveRequest(req)
		if err != nil {
			return nil, fmt.Errorf("leave request %d: %w", i, err)
		}

		entries = append(entries, entry)
	}

	return encodeJoinAuthBlobList(entries)
}

// encodeJoinAuthLeaveRequest serializes one leave request as TLV.
func encodeJoinAuthLeaveRequest(req *LeaveRequest) ([]byte, error) {
	if req.Output == nil {
		return nil, fmt.Errorf("output must be provided")
	}

	if req.Output.Value < 0 {
		return nil, fmt.Errorf("output value must be non-negative")
	}

	value := uint64(req.Output.Value)
	script := bytes.Clone(req.Output.PkScript)
	isChange := uint8(0)
	if req.IsChange {
		isChange = 1
	}

	records := []tlv.Record{
		tlv.MakePrimitiveRecord(
			joinRoundAuthLeaveValueRecordType, &value,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthLeaveScriptRecordType, &script,
		),
		tlv.MakePrimitiveRecord(
			joinRoundAuthLeaveIsChangeRecordType, &isChange,
		),
	}

	return encodeJoinAuthTLV(records)
}

// encodeJoinAuthOutPoint serializes an outpoint into hash/index
// primitives.
func encodeJoinAuthOutPoint(outpoint *wire.OutPoint) ([]byte, uint64, error) {
	if outpoint == nil {
		return nil, 0, fmt.Errorf("outpoint must be provided")
	}

	hash := bytes.Clone(outpoint.Hash[:])
	index := uint64(outpoint.Index)

	return hash, index, nil
}

// encodeJoinAuthPubKey serializes a required compressed public key.
func encodeJoinAuthPubKey(key *btcec.PublicKey) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("public key must be provided")
	}

	return key.SerializeCompressed(), nil
}

// encodeJoinAuthTLV serializes records into deterministic TLV bytes.
func encodeJoinAuthTLV(records []tlv.Record) ([]byte, error) {
	stream, err := tlv.NewStream(records...)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := stream.Encode(&buf); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// encodeJoinAuthBlobList serializes a list of byte blobs as
// varint-length-prefixed list payload.
func encodeJoinAuthBlobList(blobs [][]byte) ([]byte, error) {
	var (
		buf     bytes.Buffer
		scratch [8]byte
	)

	err := tlv.WriteVarInt(&buf, uint64(len(blobs)), &scratch)
	if err != nil {
		return nil, err
	}

	for i := 0; i < len(blobs); i++ {
		blob := blobs[i]

		err := tlv.WriteVarInt(
			&buf, uint64(len(blob)), &scratch,
		)
		if err != nil {
			return nil, err
		}

		if _, err := buf.Write(blob); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}
