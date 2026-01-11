package rounds

import (
	"fmt"
	"log/slog"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/darepo/clientconn"
)

const (
	// Subsystem is the log subsystem code for the rounds package.
	Subsystem = "RNDS"
)

// Common log attribute helpers for consistent structured logging across the
// rounds package. These helpers follow the pattern used in the client codebase.

// LogRoundID returns a structured log attribute for a round ID.
func LogRoundID(id RoundID) slog.Attr {
	return slog.String("round_id", id.String())
}

// LogPhase returns a structured log attribute for a timeout phase.
func LogPhase(phase TimeoutPhase) slog.Attr {
	return slog.String("phase", string(phase))
}

// LogState returns a structured log attribute for an FSM state name.
func LogState(state string) slog.Attr {
	return slog.String("state", state)
}

// LogEvent returns a structured log attribute for an event type.
func LogEvent(event Event) slog.Attr {
	return slog.String("event_type", fmt.Sprintf("%T", event))
}

// LogClientID returns a structured log attribute for a client ID.
func LogClientID(clientID clientconn.ClientID) slog.Attr {
	return slog.String("client_id", string(clientID))
}

// LogClientCount returns a structured log attribute for client count.
func LogClientCount(count int) slog.Attr {
	return slog.Int("client_count", count)
}

// LogOutpoint returns a structured log attribute for an outpoint.
func LogOutpoint(outpoint *wire.OutPoint) slog.Attr {
	return btclog.Fmt("outpoint", "%v", outpoint)
}

// LogInputCount returns a structured log attribute for input count.
func LogInputCount(count int) slog.Attr {
	return slog.Int("input_count", count)
}

// LogSigCount returns a structured log attribute for signature count.
func LogSigCount(count int) slog.Attr {
	return slog.Int("sig_count", count)
}

// LogNonceCount returns a structured log attribute for nonce count.
func LogNonceCount(count int) slog.Attr {
	return slog.Int("nonce_count", count)
}

// LogVTXOCount returns a structured log attribute for VTXO count.
func LogVTXOCount(count int) slog.Attr {
	return slog.Int("vtxo_count", count)
}

// LogBoardingCount returns a structured log attribute for boarding input count.
func LogBoardingCount(count int) slog.Attr {
	return slog.Int("boarding_count", count)
}

// LogLeaveCount returns a structured log attribute for leave output count.
func LogLeaveCount(count int) slog.Attr {
	return slog.Int("leave_count", count)
}

// LogSubmitted returns a structured log attribute for submitted count.
func LogSubmitted(count int) slog.Attr {
	return slog.Int("submitted", count)
}

// LogExpected returns a structured log attribute for expected count.
func LogExpected(count int) slog.Attr {
	return slog.Int("expected", count)
}

// LogTxID returns a structured log attribute for a transaction ID.
func LogTxID(txid string) slog.Attr {
	return slog.String("txid", txid)
}

// LogReason returns a structured log attribute for a failure reason.
func LogReason(reason string) slog.Attr {
	return slog.String("reason", reason)
}

// LogInputIndex returns a structured log attribute for an input index.
func LogInputIndex(index int) slog.Attr {
	return slog.Int("input_index", index)
}

// LogOutputIndex returns a structured log attribute for an output index.
func LogOutputIndex(index int) slog.Attr {
	return slog.Int("output_index", index)
}

// LogKeyCount returns a structured log attribute for signing key count.
func LogKeyCount(count int) slog.Attr {
	return slog.Int("key_count", count)
}
