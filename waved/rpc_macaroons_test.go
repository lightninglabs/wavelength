package waved

import (
	"context"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestBakeMacaroonExactURI verifies an exact URI grant authorizes only the
// requested method, even when another method has the same entity permission.
func TestBakeMacaroonExactURI(t *testing.T) {
	t.Parallel()

	adminPath := filepath.Join(t.TempDir(), "admin.macaroon")
	authService := newTestMacaroonService(
		t, adminPath, wavedMacaroonLocation, wavedRPCPermissions,
	)
	server := NewRPCServer(nil)
	server.configureMacaroonManager(authService, wavedRPCPermissions)

	getInfo := waverpc.DaemonService_GetInfo_FullMethodName
	resp, err := server.BakeMacaroon(
		context.Background(), &waverpc.BakeMacaroonRequest{
			Permissions: []*waverpc.MacaroonPermission{{
				Entity: macaroons.PermissionEntityCustomURI,
				Action: getInfo,
			}},
		},
	)
	require.NoError(t, err)

	macBytes, err := hex.DecodeString(resp.GetMacaroon())
	require.NoError(t, err)
	require.NoError(
		t,
		authService.CheckMacAuth(
			context.Background(), macBytes,
			wavedRPCPermissions[getInfo], getInfo,
		),
	)

	listPermissions :=
		waverpc.MacaroonService_ListPermissions_FullMethodName
	require.Error(
		t,
		authService.CheckMacAuth(
			context.Background(), macBytes,
			wavedRPCPermissions[listPermissions], listPermissions,
		),
	)
}

// TestBakeMacaroonValidatesPermissions verifies the baker accepts known
// entity/action pairs and rejects empty or unknown permissions.
func TestBakeMacaroonValidatesPermissions(t *testing.T) {
	t.Parallel()

	adminPath := filepath.Join(t.TempDir(), "admin.macaroon")
	authService := newTestMacaroonService(
		t, adminPath, wavedMacaroonLocation, wavedRPCPermissions,
	)
	server := NewRPCServer(nil)
	server.configureMacaroonManager(authService, wavedRPCPermissions)

	cases := []struct {
		name        string
		permissions []*waverpc.MacaroonPermission
		wantCode    codes.Code
	}{
		{
			name: "known entity action",
			permissions: []*waverpc.MacaroonPermission{{
				Entity: entityInfo,
				Action: "read",
			}},
			wantCode: codes.OK,
		},
		{
			name:     "empty",
			wantCode: codes.InvalidArgument,
		},
		{
			name: "nil permission",
			permissions: []*waverpc.MacaroonPermission{
				nil,
			},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "unknown entity action",
			permissions: []*waverpc.MacaroonPermission{{
				Entity: "unknown",
				Action: "read",
			}},
			wantCode: codes.InvalidArgument,
		},
		{
			name: "unknown URI",
			permissions: []*waverpc.MacaroonPermission{{
				Entity: macaroons.PermissionEntityCustomURI,
				Action: "/waverpc.DaemonService/Unknown",
			}},
			wantCode: codes.InvalidArgument,
		},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			resp, err := server.BakeMacaroon(
				context.Background(),
				&waverpc.BakeMacaroonRequest{
					Permissions: testCase.permissions,
				},
			)
			require.Equal(t, testCase.wantCode, status.Code(err))
			if testCase.wantCode == codes.OK {
				require.NotEmpty(t, resp.GetMacaroon())

				macBytes, err := hex.DecodeString(
					resp.GetMacaroon(),
				)
				require.NoError(t, err)
				getInfo := waverpc.
					DaemonService_GetInfo_FullMethodName
				require.NoError(
					t,
					authService.CheckMacAuth(
						context.Background(), macBytes,
						wavedRPCPermissions[getInfo],
						getInfo,
					),
				)
			}
		})
	}
}

// TestListPermissionsReturnsDefensiveMap verifies permission discovery uses
// the active map and callers cannot mutate the manager through its response.
func TestListPermissionsReturnsDefensiveMap(t *testing.T) {
	t.Parallel()

	server := NewRPCServer(nil)
	server.configureMacaroonManager(nil, wavedRPCPermissions)

	resp, err := server.ListPermissions(
		context.Background(), &waverpc.ListPermissionsRequest{},
	)
	require.NoError(t, err)

	getInfo := waverpc.DaemonService_GetInfo_FullMethodName
	require.Equal(t, []*waverpc.MacaroonPermission{{
		Entity: entityInfo,
		Action: "read",
	}}, resp.GetMethodPermissions()[getInfo].GetPermissions())

	delete(resp.GetMethodPermissions(), getInfo)
	again, err := server.ListPermissions(
		context.Background(), &waverpc.ListPermissionsRequest{},
	)
	require.NoError(t, err)
	require.Contains(t, again.GetMethodPermissions(), getInfo)
}

// TestBakeMacaroonRequiresAuthentication verifies credential creation fails
// closed when the daemon was started without macaroon authentication.
func TestBakeMacaroonRequiresAuthentication(t *testing.T) {
	t.Parallel()

	server := NewRPCServer(nil)
	_, err := server.BakeMacaroon(
		context.Background(), &waverpc.BakeMacaroonRequest{
			Permissions: []*waverpc.MacaroonPermission{{
				Entity: entityInfo,
				Action: "read",
			}},
		},
	)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}
