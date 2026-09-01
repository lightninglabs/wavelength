package waved

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

// macaroonManager validates scoped bake requests against the active daemon
// permission map and mints them with waved's macaroon root-key store.
type macaroonManager struct {
	authService *lndclient.MacaroonService
	permissions map[string][]bakery.Op
	knownOps    map[bakery.Op]struct{}
}

// newMacaroonManager returns a manager over a defensive copy of permissions.
func newMacaroonManager(authService *lndclient.MacaroonService,
	permissions map[string][]bakery.Op) *macaroonManager {

	permissionCopy := make(map[string][]bakery.Op, len(permissions))
	knownOps := make(map[bakery.Op]struct{})
	for fullMethod, ops := range permissions {
		permissionCopy[fullMethod] = append([]bakery.Op(nil), ops...)
		for _, op := range ops {
			knownOps[op] = struct{}{}
		}
	}

	return &macaroonManager{
		authService: authService,
		permissions: permissionCopy,
		knownOps:    knownOps,
	}
}

// configureMacaroonManager publishes the active permission map after every
// optional gRPC service has registered and before the listener starts.
func (r *RPCServer) configureMacaroonManager(
	authService *lndclient.MacaroonService,
	permissions map[string][]bakery.Op) {

	r.macaroonManager = newMacaroonManager(authService, permissions)
}

// BakeMacaroon creates a new daemon macaroon with a validated permission set.
func (r *RPCServer) BakeMacaroon(ctx context.Context,
	req *waverpc.BakeMacaroonRequest) (*waverpc.BakeMacaroonResponse,
	error) {

	manager := r.macaroonManager
	if manager == nil || manager.authService == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"macaroon authentication is disabled",
		)
	}

	ops, err := manager.validatePermissions(req.GetPermissions())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	rootKeyID := []byte(strconv.FormatUint(req.GetRootKeyId(), 10))
	mac, err := manager.authService.NewMacaroon(
		ctx, rootKeyID, ops...,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "bake macaroon: %v",
			err)
	}

	macBytes, err := mac.M().MarshalBinary()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal "+
			"macaroon: %v", err)
	}

	return &waverpc.BakeMacaroonResponse{
		Macaroon: hex.EncodeToString(macBytes),
	}, nil
}

// ListPermissions returns the active RPC method-to-permission map.
func (r *RPCServer) ListPermissions(_ context.Context,
	_ *waverpc.ListPermissionsRequest) (*waverpc.ListPermissionsResponse,
	error) {

	manager := r.macaroonManager
	if manager == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"macaroon permission map is unavailable",
		)
	}

	permissions := make(
		map[string]*waverpc.MacaroonPermissionList,
		len(manager.permissions),
	)
	for fullMethod, ops := range manager.permissions {
		rpcOps := make([]*waverpc.MacaroonPermission, 0, len(ops))
		for _, op := range ops {
			rpcOps = append(rpcOps, &waverpc.MacaroonPermission{
				Entity: op.Entity,
				Action: op.Action,
			})
		}

		permissions[fullMethod] = &waverpc.MacaroonPermissionList{
			Permissions: rpcOps,
		}
	}

	return &waverpc.ListPermissionsResponse{
		MethodPermissions: permissions,
	}, nil
}

// validatePermissions converts and validates user-facing permission pairs.
func (m *macaroonManager) validatePermissions(
	permissions []*waverpc.MacaroonPermission) ([]bakery.Op, error) {

	if len(permissions) == 0 {
		return nil, fmt.Errorf("permission list cannot be empty")
	}

	ops := make([]bakery.Op, 0, len(permissions))
	seen := make(map[bakery.Op]struct{}, len(permissions))
	for idx, permission := range permissions {
		if permission == nil {
			return nil, fmt.Errorf("permission %d is nil", idx)
		}

		op := bakery.Op{
			Entity: permission.GetEntity(),
			Action: permission.GetAction(),
		}
		if op.Entity == "" || op.Action == "" {
			return nil, fmt.Errorf("permission %d requires "+
				"non-empty entity and action", idx)
		}

		if op.Entity == macaroons.PermissionEntityCustomURI {
			if _, ok := m.permissions[op.Action]; !ok {
				return nil, fmt.Errorf("unknown RPC "+
					"method URI %q", op.Action)
			}
		} else if _, ok := m.knownOps[op]; !ok {
			return nil, fmt.Errorf("unsupported permission %s:%s",
				op.Entity, op.Action)
		}

		if _, ok := seen[op]; ok {
			continue
		}

		seen[op] = struct{}{}
		ops = append(ops, op)
	}

	return ops, nil
}
