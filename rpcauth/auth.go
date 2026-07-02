package rpcauth

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	macaroon "gopkg.in/macaroon.v2"
)

const (
	// MacaroonMetadataKey is the gRPC metadata and HTTP header key that
	// carries serialized macaroons.
	MacaroonMetadataKey = "macaroon"
)

// loadMacaroon loads a binary macaroon from disk.
func loadMacaroon(path string) (*macaroon.Macaroon, error) {
	serialized, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("read macaroon: %w", err)
	}

	var mac macaroon.Macaroon
	if err := mac.UnmarshalBinary(serialized); err != nil {
		return nil, fmt.Errorf("decode macaroon: %w", err)
	}

	return &mac, nil
}

// HexFromFile returns the hex-encoded serialized macaroon at path.
func HexFromFile(path string) (string, error) {
	serialized, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return "", fmt.Errorf("read macaroon: %w", err)
	}

	return hex.EncodeToString(serialized), nil
}

// DialOptionFromFile returns a gRPC dial option for macaroon auth.
func DialOptionFromFile(path string) (grpc.DialOption, error) {
	mac, err := loadMacaroon(path)
	if err != nil {
		return nil, err
	}

	creds, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf("create macaroon credentials: %w", err)
	}

	return grpc.WithPerRPCCredentials(creds), nil
}
