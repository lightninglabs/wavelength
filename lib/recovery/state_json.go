package recovery

import (
	"encoding/json"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// sessionStateJSON is the JSON-friendly representation of SessionState,
// using hex-encoded hash strings as map keys instead of [32]byte.
type sessionStateJSON struct {
	TxStates       map[string]TxState `json:"tx_states"`
	ConfirmHeights map[string]int32   `json:"confirm_heights"`
	FailedTxid     *string            `json:"failed_txid,omitempty"`
	LastError      string             `json:"last_error,omitempty"`
}

// MarshalJSON implements json.Marshaler for SessionState.
func (s *SessionState) MarshalJSON() ([]byte, error) {
	js := sessionStateJSON{
		TxStates:       make(map[string]TxState, len(s.TxStates)),
		ConfirmHeights: make(map[string]int32, len(s.ConfirmHeights)),
		LastError:      s.LastError,
	}

	for txid, state := range s.TxStates {
		js.TxStates[txid.String()] = state
	}

	for txid, height := range s.ConfirmHeights {
		js.ConfirmHeights[txid.String()] = height
	}

	if s.FailedTxid != nil {
		str := s.FailedTxid.String()
		js.FailedTxid = &str
	}

	return json.Marshal(js)
}

// UnmarshalJSON implements json.Unmarshaler for SessionState.
func (s *SessionState) UnmarshalJSON(data []byte) error {
	var js sessionStateJSON
	if err := json.Unmarshal(data, &js); err != nil {
		return err
	}

	s.TxStates = make(map[chainhash.Hash]TxState, len(js.TxStates))
	for key, state := range js.TxStates {
		hash, err := parseHash(key)
		if err != nil {
			return fmt.Errorf("tx_states key %q: %w", key, err)
		}

		s.TxStates[hash] = state
	}

	s.ConfirmHeights = make(map[chainhash.Hash]int32,
		len(js.ConfirmHeights))
	for key, height := range js.ConfirmHeights {
		hash, err := parseHash(key)
		if err != nil {
			return fmt.Errorf("confirm_heights key %q: %w",
				key, err)
		}

		s.ConfirmHeights[hash] = height
	}

	if js.FailedTxid != nil {
		hash, err := parseHash(*js.FailedTxid)
		if err != nil {
			return fmt.Errorf("failed_txid %q: %w",
				*js.FailedTxid, err)
		}

		s.FailedTxid = &hash
	}

	s.LastError = js.LastError

	return nil
}

// parseHash decodes a hex-encoded hash string produced by
// chainhash.Hash.String() back into a chainhash.Hash. String()
// returns the hash in reversed byte order (Bitcoin display format),
// so we must use NewHashFromStr which reverses the bytes on decode.
func parseHash(s string) (chainhash.Hash, error) {
	hash, err := chainhash.NewHashFromStr(s)
	if err != nil {
		return chainhash.Hash{}, fmt.Errorf(
			"invalid hash %q: %w", s, err)
	}

	return *hash, nil
}
