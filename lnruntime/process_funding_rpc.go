package lnruntime

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/wavelength/arkchannel"
	"github.com/lightninglabs/wavelength/rpc/arkchannelrpc"
)

const recoveryPackageInstalledEvent = arkchannelrpc.ChannelEventType_CHANNEL_EVENT_TYPE_RECOVERY_PACKAGE_INSTALLED //nolint:ll

// channelTermsToRPC serializes immutable Ark channel terms.
func channelTermsToRPC(terms arkchannel.Terms) *arkchannelrpc.ChannelTerms {
	return &arkchannelrpc.ChannelTerms{
		ChannelId: terms.ID[:], Kind: uint32(terms.Kind),
		Funder:           uint32(terms.Funder),
		PendingChannelId: terms.PendingChannelID[:],
		ReservedScid:     terms.ReservedSCID, CapacitySat: int64(
			terms.Capacity,
		),
		ClientNodeKey: terms.ClientNodeKey[:],
		HubNodeKey:    terms.HubNodeKey[:],
		PaymentHash:   terms.PaymentHash[:],
		Vtxo: &arkchannelrpc.ChannelVTXOTerms{
			ClientArkKey:     terms.VTXO.ClientArkKey[:],
			HubArkKey:        terms.VTXO.HubArkKey[:],
			ArkOperatorKey:   terms.VTXO.ArkOperatorKey[:],
			ClientChannelKey: terms.VTXO.ClientChannelKey[:],
			HubChannelKey:    terms.VTXO.HubChannelKey[:],
			FunderKey:        terms.VTXO.FunderKey[:],
			ChannelDelay:     terms.VTXO.ChannelDelay,
			FunderDelay:      terms.VTXO.FunderDelay,
			MinExitDelay:     terms.VTXO.MinExitDelay,
		},
	}
}

// ChannelTermsToRPC serializes immutable channel terms for daemon adapters.
func ChannelTermsToRPC(terms arkchannel.Terms) *arkchannelrpc.ChannelTerms {
	return channelTermsToRPC(terms)
}

// channelTermsFromRPC parses and validates immutable Ark channel terms.
func channelTermsFromRPC(message *arkchannelrpc.ChannelTerms) (arkchannel.Terms,
	error) {

	if message == nil || message.GetVtxo() == nil {
		return arkchannel.Terms{}, fmt.Errorf("complete channel " +
			"terms are required")
	}
	capacity, err := positiveRPCAmount(
		"channel capacity", message.GetCapacitySat(),
	)
	if err != nil {
		return arkchannel.Terms{}, err
	}
	var terms arkchannel.Terms
	fields := map[string]struct {
		destination []byte
		value       []byte
	}{
		"channel ID": {
			terms.ID[:],
			message.GetChannelId(),
		},
		"pending channel ID": {
			terms.PendingChannelID[:],
			message.GetPendingChannelId(),
		},
		"client node key": {
			terms.ClientNodeKey[:], message.GetClientNodeKey(),
		},
		"hub node key": {
			terms.HubNodeKey[:],
			message.GetHubNodeKey(),
		},
		"payment hash": {
			terms.PaymentHash[:],
			message.GetPaymentHash(),
		},
		"client Ark key": {
			terms.VTXO.ClientArkKey[:], message.
				GetVtxo().
				GetClientArkKey(),
		},
		"hub Ark key": {
			terms.VTXO.HubArkKey[:], message.
				GetVtxo().
				GetHubArkKey(),
		},
		"Ark operator key": {
			terms.VTXO.ArkOperatorKey[:],
			message.GetVtxo().GetArkOperatorKey(),
		},
		"client channel key": {
			terms.VTXO.ClientChannelKey[:],
			message.GetVtxo().GetClientChannelKey(),
		},
		"hub channel key": {
			terms.VTXO.HubChannelKey[:],
			message.GetVtxo().GetHubChannelKey(),
		},
		"funder key": {
			terms.VTXO.FunderKey[:], message.
				GetVtxo().
				GetFunderKey(),
		},
	}
	for name, field := range fields {
		if err := copyFixed(
			name, field.destination, field.value,
		); err != nil {
			return arkchannel.Terms{}, err
		}
	}
	terms.Kind = arkchannel.Kind(message.GetKind())
	terms.Funder = arkchannel.Party(message.GetFunder())
	terms.ReservedSCID = message.GetReservedScid()
	terms.Capacity = capacity
	terms.VTXO.ChannelDelay = message.GetVtxo().GetChannelDelay()
	terms.VTXO.FunderDelay = message.GetVtxo().GetFunderDelay()
	terms.VTXO.MinExitDelay = message.GetVtxo().GetMinExitDelay()

	return terms, terms.Validate()
}

// ChannelTermsFromRPC parses immutable channel terms for daemon adapters.
func ChannelTermsFromRPC(message *arkchannelrpc.ChannelTerms) (arkchannel.Terms,
	error) {

	return channelTermsFromRPC(message)
}

// channelBindingToRPC serializes the exact prepared OOR output.
func channelBindingToRPC(
	binding arkchannel.VTXOBinding) *arkchannelrpc.ChannelVTXOBinding {

	return &arkchannelrpc.ChannelVTXOBinding{
		OorSessionId:   binding.OORSessionID[:],
		OutpointTxid:   binding.OutPoint.Hash[:],
		OutpointIndex:  binding.OutPoint.Index,
		AmountSat:      int64(binding.Amount),
		ArkTransaction: append([]byte(nil), binding.ArkTransaction...),
		PolicyTemplate: append([]byte(nil), binding.PolicyTemplate...),
		PkScript:       append([]byte(nil), binding.PkScript...),
	}
}

// ChannelBindingToRPC serializes a prepared channel-policy VTXO binding.
func ChannelBindingToRPC(
	binding arkchannel.VTXOBinding) *arkchannelrpc.ChannelVTXOBinding {

	return channelBindingToRPC(binding)
}

// channelBindingFromRPC parses one prepared OOR binding.
func channelBindingFromRPC(message *arkchannelrpc.ChannelVTXOBinding) (
	arkchannel.VTXOBinding, error) {

	if message == nil {
		return arkchannel.VTXOBinding{}, fmt.Errorf("channel VTXO " +
			"binding is required")
	}
	amount, err := positiveRPCAmount(
		"channel VTXO amount", message.GetAmountSat(),
	)
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	hash, err := rpcHash("channel VTXO txid", message.GetOutpointTxid())
	if err != nil {
		return arkchannel.VTXOBinding{}, err
	}
	var session [32]byte
	if err := copyFixed(
		"OOR session ID", session[:], message.GetOorSessionId(),
	); err != nil {
		return arkchannel.VTXOBinding{}, err
	}

	return arkchannel.VTXOBinding{
		OORSessionID: session,
		OutPoint: wire.OutPoint{
			Hash: hash, Index: message.GetOutpointIndex(),
		},
		Amount: amount,
		ArkTransaction: append(
			[]byte(nil), message.GetArkTransaction()...,
		),
		PolicyTemplate: append(
			[]byte(nil), message.GetPolicyTemplate()...,
		),
		PkScript: append([]byte(nil), message.GetPkScript()...),
	}, nil
}

// ChannelBindingFromRPC parses a prepared channel-policy VTXO binding.
func ChannelBindingFromRPC(message *arkchannelrpc.ChannelVTXOBinding) (
	arkchannel.VTXOBinding, error) {

	return channelBindingFromRPC(message)
}

// channelEventToRPC serializes the funding barriers used by the negotiator.
func channelEventToRPC(id arkchannel.ID, event arkchannel.Event) (
	*arkchannelrpc.ApplyChannelEventRequest, string, error) {

	request := &arkchannelrpc.ApplyChannelEventRequest{ChannelId: id[:]}
	switch event := event.(type) {
	case *arkchannel.FundingPeerReady:
		request.EventType = arkchannelrpc.
			ChannelEventType_CHANNEL_EVENT_TYPE_FUNDING_PEER_READY

		return request, "funding-peer-ready", nil

	case *arkchannel.FundingFinalized:
		request.EventType = arkchannelrpc.
			ChannelEventType_CHANNEL_EVENT_TYPE_FUNDING_FINALIZED
		request.Party = uint32(event.Party)

		return request, "funding-finalized/" + event.Party.String(), nil

	case *arkchannel.OORFinalized:
		request.EventType = arkchannelrpc.
			ChannelEventType_CHANNEL_EVENT_TYPE_OOR_FINALIZED
		request.OorSessionId = event.SessionID[:]

		return request, "oor-finalized", nil

	case *arkchannel.OORAborted:
		if event.Reason == "" {
			return nil, "", fmt.Errorf("OOR abort reason is " +
				"required")
		}
		request.EventType = arkchannelrpc.
			ChannelEventType_CHANNEL_EVENT_TYPE_OOR_ABORTED
		request.OorSessionId = event.SessionID[:]
		request.FailureReason = event.Reason

		return request, "oor-aborted", nil

	case *arkchannel.RecoveryPackageInstalled:
		request.EventType = recoveryPackageInstalledEvent
		request.Party = uint32(event.Party)

		return request, "recovery-package-installed", nil

	default:
		return nil, "", fmt.Errorf("unsupported remote channel "+
			"event %T", event)
	}
}

// channelEventFromRPC parses one supported funding barrier.
func channelEventFromRPC(request *arkchannelrpc.ApplyChannelEventRequest) (
	arkchannel.Event, error) {

	switch request.GetEventType() {
	case arkchannelrpc.
		ChannelEventType_CHANNEL_EVENT_TYPE_FUNDING_PEER_READY:
		return &arkchannel.FundingPeerReady{}, nil

	case arkchannelrpc.
		ChannelEventType_CHANNEL_EVENT_TYPE_FUNDING_FINALIZED:

		party := arkchannel.Party(request.GetParty())
		if party != arkchannel.PartyClient &&
			party != arkchannel.PartyHub {
			return nil, fmt.Errorf("invalid funding party %d",
				party)
		}

		return &arkchannel.FundingFinalized{Party: party}, nil

	case arkchannelrpc.ChannelEventType_CHANNEL_EVENT_TYPE_OOR_FINALIZED:
		var session [32]byte
		if err := copyFixed(
			"OOR session ID", session[:], request.GetOorSessionId(),
		); err != nil {
			return nil, err
		}

		return &arkchannel.OORFinalized{SessionID: session}, nil

	case arkchannelrpc.ChannelEventType_CHANNEL_EVENT_TYPE_OOR_ABORTED:
		var session [32]byte
		if err := copyFixed(
			"OOR session ID", session[:], request.GetOorSessionId(),
		); err != nil {
			return nil, err
		}
		if request.GetFailureReason() == "" {
			return nil, fmt.Errorf("OOR abort reason is required")
		}

		return &arkchannel.OORAborted{
			SessionID: session,
			Reason:    request.GetFailureReason(),
		}, nil

	case arkchannelrpc.
		ChannelEventType_CHANNEL_EVENT_TYPE_RECOVERY_PACKAGE_INSTALLED:

		party := arkchannel.Party(request.GetParty())
		if party != arkchannel.PartyClient &&
			party != arkchannel.PartyHub {
			return nil, fmt.Errorf("invalid recovery party %d",
				party)
		}

		return &arkchannel.RecoveryPackageInstalled{Party: party}, nil

	default:
		return nil, fmt.Errorf("unsupported channel event type %d",
			request.GetEventType())
	}
}

// positiveRPCAmount parses a strictly positive signed wire amount.
func positiveRPCAmount(name string, value int64) (btcutil.Amount, error) {
	amount, err := rpcAmount(name, value)
	if err != nil {
		return 0, err
	}
	if amount == 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}

	return amount, nil
}

// copyFixed copies one exact-width byte field.
func copyFixed(name string, destination, source []byte) error {
	if len(source) != len(destination) {
		return fmt.Errorf("%s must be %d bytes", name, len(destination))
	}
	copy(destination, source)

	return nil
}
