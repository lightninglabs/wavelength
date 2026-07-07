package oor

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
)

const incomingResolveCorrelationPrefix = "oor-incoming-resolve:"

// NewResolveIncomingTransferRequest converts a lightweight IncomingOOREvent
// notification into a durable actor request that can be persisted without
// blocking mailbox ingress on a follow-up indexer query.
func NewResolveIncomingTransferRequest(evt *arkrpc.IncomingOOREvent) (
	*ResolveIncomingTransferRequest, error) {

	if evt == nil {
		return nil, fmt.Errorf("nil IncomingOOREvent")
	}

	sessionID, err := chainhash.NewHash(evt.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("parse session id: %w", err)
	}

	return &ResolveIncomingTransferRequest{
		SessionID: SessionID(*sessionID),
		RecipientPkScript: append(
			[]byte(nil), evt.GetRecipientPkScript()...,
		),
		RecipientEventID: evt.GetRecipientEventId(),
	}, nil
}

// IncomingResolveCorrelationID returns the stable unary correlation ID used
// for durable incoming-transfer resolution queries for the given session and
// recipient event.
func IncomingResolveCorrelationID(sessionID SessionID,
	recipientEventID uint64) string {

	return incomingResolveCorrelationPrefix +
		chainhash.Hash(sessionID).String() + ":" +
		strconv.FormatUint(recipientEventID, 10)
}

// IsIncomingResolveCorrelationID returns true when correlationID belongs to a
// durable incoming-transfer resolution query.
func IsIncomingResolveCorrelationID(correlationID string) bool {
	return len(correlationID) > len(incomingResolveCorrelationPrefix) &&
		correlationID[:len(incomingResolveCorrelationPrefix)] ==
			incomingResolveCorrelationPrefix
}

// ParseIncomingResolveCorrelationID decodes a durable incoming-transfer
// resolution query correlation ID back into the OOR session ID and recipient
// event ID.
func ParseIncomingResolveCorrelationID(correlationID string) (SessionID, uint64,
	error) {

	if !IsIncomingResolveCorrelationID(correlationID) {
		return SessionID{}, 0, fmt.Errorf("unexpected incoming "+
			"resolve correlation id: %q", correlationID)
	}

	suffix := correlationID[len(incomingResolveCorrelationPrefix):]
	parts := strings.SplitN(suffix, ":", 2)
	if len(parts) != 2 {
		return SessionID{}, 0, fmt.Errorf("unexpected incoming "+
			"resolve correlation id payload: %q", suffix)
	}

	hash, err := chainhash.NewHashFromStr(parts[0])
	if err != nil {
		return SessionID{}, 0, fmt.Errorf("parse incoming resolve "+
			"session id: %w", err)
	}

	recipientEventID, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return SessionID{}, 0, fmt.Errorf("parse incoming resolve "+
			"event id: %w", err)
	}

	return SessionID(*hash), recipientEventID, nil
}

// IncomingTransferEventFromResponse validates and converts one
// ListOORRecipientEventsByScriptResponse payload into the complete incoming
// transfer event expected by the receive FSM.
func IncomingTransferEventFromResponse(sessionID SessionID,
	recipientEventID uint64,
	resp *arkrpc.ListOORRecipientEventsByScriptResponse) (
	*IncomingTransferEvent, error) {

	return IncomingTransferEventFromResponseWithLimits(
		sessionID, recipientEventID, resp, ReceiveLimits{},
	)
}

// IncomingTransferEventFromResponseWithLimits validates and converts one
// ListOORRecipientEventsByScriptResponse payload using the supplied
// defense-in-depth limits. Zero limit fields use package defaults.
func IncomingTransferEventFromResponseWithLimits(sessionID SessionID,
	recipientEventID uint64,
	resp *arkrpc.ListOORRecipientEventsByScriptResponse,
	limits ReceiveLimits) (*IncomingTransferEvent, error) {

	if resp == nil {
		return nil, fmt.Errorf("incoming transfer response must be " +
			"provided")
	}

	if len(resp.GetEvents()) == 0 {
		return nil, fmt.Errorf("no events found for session %x", //nolint:ll
			sessionID[:])
	}

	recipientEvt := resp.Events[0]
	if recipientEvt == nil {
		return nil, fmt.Errorf("incoming transfer event must be " +
			"provided")
	}

	if recipientEvt.GetEventId() != recipientEventID {
		return nil, fmt.Errorf("unexpected recipient event id: got "+
			"%d, want %d", recipientEvt.GetEventId(),
			recipientEventID)
	}

	eventSessionID, err := chainhash.NewHash(recipientEvt.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("parse event session id: %w", err)
	}
	if SessionID(*eventSessionID) != sessionID {
		return nil, fmt.Errorf("incoming transfer session mismatch")
	}

	arkPSBT, err := psbtutil.Parse(recipientEvt.GetArkPsbt())
	if err != nil {
		return nil, fmt.Errorf("parse ark psbt: %w", err)
	}

	limits = normalizeReceiveLimits(limits)
	if uint64(len(recipientEvt.GetCheckpointPsbts())) >
		uint64(limits.MaxCheckpoints) {
		return nil, fmt.Errorf("max checkpoints exceeded: checkpoint "+
			"count %d exceeds limit %d",
			len(recipientEvt.GetCheckpointPsbts()),
			limits.MaxCheckpoints)
	}

	checkpoints := make(
		[]*psbt.Packet, 0,
		len(
			recipientEvt.GetCheckpointPsbts(),
		),
	)
	for _, cpRaw := range recipientEvt.GetCheckpointPsbts() {
		cp, cpErr := psbtutil.Parse(cpRaw)
		if cpErr != nil {
			return nil, fmt.Errorf("parse checkpoint: %w", cpErr)
		}

		checkpoints = append(checkpoints, cp)
	}

	ancestors, err := packageArtifactsFromRPC(
		recipientEvt.GetAncestorPackages(), limits,
	)
	if err != nil {
		return nil, err
	}

	recipients, err := incomingRecipientsFromEvent(arkPSBT, recipientEvt)
	if err != nil {
		return nil, err
	}

	root := packageArtifactForValidation(sessionID, arkPSBT, checkpoints)
	err = validateIncomingPackageGraph(root, ancestors)
	if err != nil {
		return nil, err
	}

	return &IncomingTransferEvent{
		SessionID:            sessionID,
		ArkPSBT:              arkPSBT,
		FinalCheckpointPSBTs: checkpoints,
		AncestorPackages:     ancestors,
		Recipients:           recipients,
	}, nil
}

// incomingRecipientsFromEvent overlays the policy metadata carried by the
// recipient event onto the structurally extracted Ark outputs.
func incomingRecipientsFromEvent(ark *psbt.Packet,
	evt *arkrpc.OORRecipientEvent) ([]ArkRecipientOutput, error) {

	recipients, err := ExtractArkRecipients(ark)
	if err != nil {
		return nil, err
	}

	if evt.GetValue() > uint64(math.MaxInt64) {
		return nil, fmt.Errorf("recipient event value overflows int64")
	}

	value := btcutil.Amount(evt.GetValue())
	for i := range recipients {
		if recipients[i].OutputIndex != evt.GetOutputIndex() {
			continue
		}

		if recipients[i].Value != value {
			return nil, fmt.Errorf("recipient event value mismatch")
		}

		if !bytes.Equal(
			recipients[i].PkScript, evt.GetRecipientPkScript(),
		) {
			return nil, fmt.Errorf("recipient event pkscript " +
				"mismatch")
		}

		recipients[i].VTXOPolicyTemplate = append(
			[]byte(nil), evt.GetVtxoPolicyTemplate()...,
		)

		return recipients, nil
	}

	return nil, fmt.Errorf("recipient event output %d not found",
		evt.GetOutputIndex())
}

// packageArtifactsFromRPC converts RPC package artifacts into domain
// artifacts after enforcing the same bounded-shape policy as checkpoint
// parsing.
func packageArtifactsFromRPC(pkgs []*arkrpc.OORSessionPackage,
	limits ReceiveLimits) ([]PackageArtifact, error) {

	const maxAncestorPackages = 64
	if len(pkgs) > maxAncestorPackages {
		return nil, fmt.Errorf("ancestor package count %d exceeds "+
			"limit %d", len(pkgs), maxAncestorPackages)
	}

	limits = normalizeReceiveLimits(limits)
	artifacts := make([]PackageArtifact, 0, len(pkgs))
	for i := range pkgs {
		pkg := pkgs[i]
		if pkg == nil {
			return nil, fmt.Errorf("ancestor package %d is nil", i)
		}

		sessionID, err := chainhash.NewHash(pkg.GetSessionId())
		if err != nil {
			return nil, fmt.Errorf("parse ancestor package "+
				"session id %d: %w", i, err)
		}

		arkPSBT, err := psbtutil.Parse(pkg.GetArkPsbt())
		if err != nil {
			return nil, fmt.Errorf("parse ancestor package ark "+
				"psbt %d: %w", i, err)
		}

		if uint64(len(pkg.GetCheckpointPsbts())) >
			uint64(limits.MaxCheckpoints) {
			return nil, fmt.Errorf("ancestor package %d "+
				"checkpoint count %d exceeds limit %d", i,
				len(pkg.GetCheckpointPsbts()),
				limits.MaxCheckpoints)
		}

		checkpoints := make(
			[]*psbt.Packet, 0,
			len(
				pkg.GetCheckpointPsbts(),
			),
		)
		for j, cpRaw := range pkg.GetCheckpointPsbts() {
			cp, err := psbtutil.Parse(cpRaw)
			if err != nil {
				return nil, fmt.Errorf("parse ancestor "+
					"package %d checkpoint %d: %w", i, j,
					err)
			}

			checkpoints = append(checkpoints, cp)
		}

		artifacts = append(artifacts, PackageArtifact{
			SessionID:            SessionID(*sessionID),
			ArkPSBT:              arkPSBT,
			FinalCheckpointPSBTs: checkpoints,
		})
	}

	return artifacts, nil
}
