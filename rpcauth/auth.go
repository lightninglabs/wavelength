package rpcauth

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
	macaroon "gopkg.in/macaroon.v2"
)

const (
	// MacaroonMetadataKey is the gRPC metadata and HTTP header key that
	// carries serialized macaroons.
	MacaroonMetadataKey = "macaroon"

	defaultMacaroonDBTimeout = 5 * time.Second
)

// macDbDefaultPw satisfies lndclient's root-key store API. The local RPC
// security boundary is the daemon's 0700 data directory and 0600 macaroon/root
// key files, not secrecy of this process-wide constant.
var macDbDefaultPw = []byte("darepo macaroon db")

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

// Service verifies macaroon metadata against a permission map.
type Service struct {
	mu         sync.RWMutex
	db         kvdb.Backend
	auth       *lndclient.MacaroonService
	permission map[string][]bakery.Op
}

// NewService creates a macaroon auth service.
func NewService(dbPath, macaroonPath, location string, defaultOps []bakery.Op,
	permission map[string][]bakery.Op) (*Service, error) {

	if dbPath == "" {
		return nil, fmt.Errorf("macaroon db path is required")
	}
	if macaroonPath == "" {
		return nil, fmt.Errorf("macaroon path is required")
	}
	if location == "" {
		return nil, fmt.Errorf("macaroon location is required")
	}
	if len(defaultOps) == 0 {
		return nil, fmt.Errorf("default macaroon permissions are " +
			"required")
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("create macaroon db dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(macaroonPath), 0o700); err != nil {
		return nil, fmt.Errorf("create macaroon dir: %w", err)
	}

	rootKeyStore, db, err := lndclient.NewBoltMacaroonStore(
		filepath.Dir(dbPath), filepath.Base(dbPath),
		defaultMacaroonDBTimeout,
	)
	if err != nil {
		return nil, err
	}

	auth, err := lndclient.NewMacaroonService(
		&lndclient.MacaroonServiceConfig{
			RootKeyStore:     rootKeyStore,
			MacaroonLocation: location,
			MacaroonPath:     macaroonPath,
			Checkers: []macaroons.Checker{
				macaroons.IPLockChecker,
				macaroons.IPRangeLockChecker,
			},
			RequiredPerms: defaultPermissionMap(defaultOps),
			DBPassword:    macDbDefaultPw,
		},
	)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("create macaroon service: %w", err)
	}
	if err := auth.Start(); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("start macaroon service: %w", err)
	}
	if err := os.Chmod(macaroonPath, 0o600); err != nil {
		_ = auth.Stop()
		_ = db.Close()

		return nil, fmt.Errorf("chmod macaroon: %w", err)
	}

	return &Service{
		db:         db,
		auth:       auth,
		permission: copyPermissions(permission),
	}, nil
}

// Close stops the macaroon service and closes its backing DB.
func (s *Service) Close() error {
	var closeErr error
	if s.auth != nil {
		closeErr = errors.Join(closeErr, s.auth.Stop())
	}
	if s.db != nil {
		closeErr = errors.Join(closeErr, s.db.Close())
	}

	return closeErr
}

// SetPermissions replaces the service's method permission map.
func (s *Service) SetPermissions(permission map[string][]bakery.Op) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.permission = copyPermissions(permission)
}

// UnaryServerInterceptor returns a macaroon auth unary interceptor.
func (s *Service) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{},
		info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (
		interface{}, error) {

		if err := s.authorize(ctx, info.FullMethod); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// StreamServerInterceptor returns a macaroon auth stream interceptor.
func (s *Service) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		if err := s.authorize(
			stream.Context(), info.FullMethod,
		); err != nil {
			return err
		}

		return handler(srv, stream)
	}
}

// authorize checks that the request macaroon grants fullMethod access.
func (s *Service) authorize(ctx context.Context, fullMethod string) error {
	s.mu.RLock()
	required, ok := s.permission[fullMethod]
	required = append([]bakery.Op(nil), required...)
	s.mu.RUnlock()

	if !ok || len(required) == 0 {
		return fmt.Errorf("%s: unknown macaroon permissions required",
			fullMethod)
	}

	return s.auth.ValidateMacaroon(ctx, required, fullMethod)
}

// defaultPermissionMap builds the permission set used to generate the default
// macaroon file. The method name is not served; lndclient only needs a method
// keyed map so it can de-duplicate the operation list.
func defaultPermissionMap(ops []bakery.Op) map[string][]bakery.Op {
	return map[string][]bakery.Op{
		"/rpcauth.Default/All": append([]bakery.Op(nil), ops...),
	}
}

// copyPermissions returns a deep copy of the method permission map.
func copyPermissions(permission map[string][]bakery.Op) map[string][]bakery.Op {
	copied := make(map[string][]bakery.Op, len(permission))
	for method, ops := range permission {
		copied[method] = append([]bakery.Op(nil), ops...)
	}

	return copied
}
