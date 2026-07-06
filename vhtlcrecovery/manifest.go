package vhtlcrecovery

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	// ManifestLabelPrefix identifies indexer receive-script labels that
	// carry vHTLC recovery metadata.
	ManifestLabelPrefix = "vhtlc-recovery-v1:"

	// ManifestRoleSender identifies the local vHTLC sender/refunder key.
	ManifestRoleSender = "sender"

	// ManifestRoleReceiver identifies the local vHTLC receiver/claimer key.
	ManifestRoleReceiver = "receiver"

	// ManifestDirectionPay is the client-funded Ark-to-Lightning swap path.
	ManifestDirectionPay = "pay"
)

// RecoveryManifest is the compact, indexer-stored metadata needed to rebuild a
// wallet-owned vHTLC recovery candidate after seed restore.
type RecoveryManifest struct {
	Role                                 string `json:"r"`
	Direction                            string `json:"d"`
	PaymentHash                          []byte `json:"h"`
	SenderPubkey                         []byte `json:"s"`
	ReceiverPubkey                       []byte `json:"rcv"`
	ServerPubkey                         []byte `json:"op"`
	RefundLocktime                       uint32 `json:"rl"`
	UnilateralClaimDelay                 uint32 `json:"uc"`
	UnilateralRefundDelay                uint32 `json:"ur"`
	UnilateralRefundWithoutReceiverDelay uint32 `json:"urwr"`
	PkScript                             []byte `json:"ps,omitempty"`
	AmountSat                            int64  `json:"amt,omitempty"`
	SignerKeyFamily                      int32  `json:"kf"`
	SignerKeyIndex                       int32  `json:"ki"`
	StatusHint                           string `json:"hint,omitempty"`
}

// EncodeManifestLabel serializes a recovery manifest into an indexer label.
func EncodeManifestLabel(manifest RecoveryManifest) (string, error) {
	if err := manifest.validate(); err != nil {
		return "", err
	}

	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal vhtlc recovery manifest: %w",
			err)
	}

	return ManifestLabelPrefix + base64.RawURLEncoding.EncodeToString(
		encoded,
	), nil
}

// DecodeManifestLabel parses a recovery manifest label. The boolean return is
// false when the label belongs to another receive-script use.
func DecodeManifestLabel(label string) (RecoveryManifest, bool, error) {
	if !strings.HasPrefix(label, ManifestLabelPrefix) {
		return RecoveryManifest{}, false, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(
		strings.TrimPrefix(label, ManifestLabelPrefix),
	)
	if err != nil {
		return RecoveryManifest{}, true, fmt.Errorf("decode vhtlc "+
			"recovery manifest: %w", err)
	}

	var manifest RecoveryManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return RecoveryManifest{}, true, fmt.Errorf("unmarshal vhtlc "+
			"recovery manifest: %w", err)
	}
	if err := manifest.validate(); err != nil {
		return RecoveryManifest{}, true, err
	}

	return manifest, true, nil
}

// validate rejects labels that do not carry enough data to rebuild a recovery
// policy safely.
func (m RecoveryManifest) validate() error {
	switch {
	case m.Role == "":
		return fmt.Errorf("vhtlc recovery manifest role is required")

	case m.Direction == "":
		return fmt.Errorf("vhtlc recovery manifest direction is " +
			"required")

	case len(m.PaymentHash) != 32:
		return fmt.Errorf("vhtlc recovery manifest payment hash " +
			"must be 32 bytes")

	case len(m.SenderPubkey) == 0:
		return fmt.Errorf("vhtlc recovery manifest sender pubkey is " +
			"required")

	case len(m.ReceiverPubkey) == 0:
		return fmt.Errorf("vhtlc recovery manifest receiver " +
			"pubkey is required")

	case len(m.ServerPubkey) == 0:
		return fmt.Errorf("vhtlc recovery manifest server pubkey is " +
			"required")

	case m.RefundLocktime == 0:
		return fmt.Errorf("vhtlc recovery manifest refund " +
			"locktime is required")

	case m.UnilateralClaimDelay == 0:
		return fmt.Errorf("vhtlc recovery manifest claim delay is " +
			"required")

	case m.UnilateralRefundDelay == 0:
		return fmt.Errorf("vhtlc recovery manifest refund delay is " +
			"required")

	case m.UnilateralRefundWithoutReceiverDelay == 0:
		return fmt.Errorf("vhtlc recovery manifest refund-without-" +
			"receiver delay is required")

	case len(m.PkScript) == 0:
		return fmt.Errorf("vhtlc recovery manifest pk script is " +
			"required")
	}

	return nil
}
