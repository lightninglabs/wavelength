package round

import (
	"fmt"

	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"google.golang.org/protobuf/proto"
)

// parseDurableRoundID requires all bytes, including an explicitly zero ID.
func parseDurableRoundID(raw []byte) (RoundID, error) {
	var id RoundID
	if len(raw) != len(id) {
		return id, fmt.Errorf("invalid durable round ID")
	}
	copy(id[:], raw)

	return id, nil
}

// encodeDurableQuote preserves the binding quote with its owning round ID.
func encodeDurableQuote(id RoundID, v *ClientQuote) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	p := &roundpb.JoinRoundQuote{
		RoundId:        id.String(),
		QuoteId:        v.QuoteID[:],
		SealPassNumber: v.SealPass,
		OperatorFeeSat: v.OperatorFeeSat,
		QuoteExpiresAt: v.QuoteExpiresAt,
		RejectReason:   v.RejectReason,
	}
	for _, quote := range v.VTXOQuotes {
		p.VtxoQuotes = append(p.VtxoQuotes, &roundpb.VTXOQuote{
			PkScript:     quote.PkScript,
			AmountSat:    quote.AmountSat,
			RecipientKey: quote.RecipientKey,
		})
	}
	for _, quote := range v.LeaveQuotes {
		p.LeaveQuotes = append(p.LeaveQuotes, &roundpb.LeaveQuote{
			PkScript:  quote.PkScript,
			AmountSat: quote.AmountSat,
		})
	}

	return proto.MarshalOptions{Deterministic: true}.Marshal(p)
}

// decodeDurableQuote applies the same shape validation as a live server quote.
func decodeDurableQuote(id RoundID, raw []byte) (*ClientQuote, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var p roundpb.JoinRoundQuote
	if err := proto.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	var event JoinRoundQuoteReceived
	if err := event.FromProto(&p); err != nil {
		return nil, err
	}
	if event.RoundID != id {
		return nil, fmt.Errorf("durable quote round mismatch")
	}

	return event.Quote, nil
}
