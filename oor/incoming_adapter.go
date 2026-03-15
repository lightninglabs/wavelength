package oor

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/darepo-client/arkrpc"
	"github.com/lightninglabs/darepo-client/lib/tx/psbtutil"
)

// IncomingOORFetcher queries the indexer for full OOR package data
// given a lightweight notification hint.
type IncomingOORFetcher func(ctx context.Context,
	pkScript []byte, afterEventID uint64,
	limit uint32) (*arkrpc.ListOORRecipientEventsByScriptResponse, error)

// AdaptIncomingOOREvent converts a lightweight IncomingOOREvent
// notification into a DriveEventRequest by querying the indexer for
// the full Ark PSBT and checkpoint data.
//
// This function implements the notification→query pattern:
//  1. Parse session ID from the notification
//  2. Query indexer for the OOR recipient event with Ark PSBT
//  3. Parse Ark PSBT and checkpoint PSBTs from the response
//  4. Construct IncomingTransferEvent with full data
//
// It is used by both the production darepod route handler and the
// systest route handler to ensure identical behavior.
func AdaptIncomingOOREvent(ctx context.Context,
	evt *arkrpc.IncomingOOREvent,
	fetcher IncomingOORFetcher) (*DriveEventRequest, error) {

	if evt == nil {
		return nil, fmt.Errorf("nil IncomingOOREvent")
	}

	sessionID, err := chainhash.NewHash(evt.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("parse session id: %w", err)
	}

	// Query the indexer for the full package data. The
	// notification is just a hint — the query returns the Ark
	// PSBT and checkpoint PSBTs.
	resp, err := fetcher(
		ctx,
		evt.GetRecipientPkScript(),
		evt.GetRecipientEventId()-1,
		1,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"fetch OOR package for session %x: %w",
			sessionID[:], err,
		)
	}

	if len(resp.GetEvents()) == 0 {
		return nil, fmt.Errorf(
			"no events found for session %x",
			sessionID[:],
		)
	}

	recipientEvt := resp.Events[0]

	// Parse the Ark PSBT from the query response.
	arkPSBT, err := psbtutil.Parse(recipientEvt.GetArkPsbt())
	if err != nil {
		return nil, fmt.Errorf("parse ark psbt: %w", err)
	}

	// Parse checkpoint PSBTs.
	var checkpoints []*psbt.Packet
	for _, cpRaw := range recipientEvt.GetCheckpointPsbts() {
		cp, cpErr := psbtutil.Parse(cpRaw)
		if cpErr != nil {
			return nil, fmt.Errorf(
				"parse checkpoint: %w", cpErr,
			)
		}

		checkpoints = append(checkpoints, cp)
	}

	return &DriveEventRequest{
		SessionID: SessionID(*sessionID),
		Event: &IncomingTransferEvent{
			SessionID:            SessionID(*sessionID),
			ArkPSBT:              arkPSBT,
			FinalCheckpointPSBTs: checkpoints,
		},
	}, nil
}
