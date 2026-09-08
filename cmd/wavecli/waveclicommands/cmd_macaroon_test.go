package waveclicommands

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/waverpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"
)

type macaroonClientStub struct {
	bake func(*waverpc.BakeMacaroonRequest) *waverpc.BakeMacaroonResponse
}

// BakeMacaroon records and serves a test bake request.
func (s *macaroonClientStub) BakeMacaroon(_ context.Context,
	req *waverpc.BakeMacaroonRequest, _ ...grpc.CallOption) (
	*waverpc.BakeMacaroonResponse, error) {

	return s.bake(req), nil
}

// ListPermissions fulfills the generated client interface for CLI tests.
func (*macaroonClientStub) ListPermissions(context.Context,
	*waverpc.ListPermissionsRequest, ...grpc.CallOption) (
	*waverpc.ListPermissionsResponse, error) {

	return &waverpc.ListPermissionsResponse{}, nil
}

// TestParseMacaroonPermissions verifies exact URI actions retain their full
// method path while malformed tuples fail locally.
func TestParseMacaroonPermissions(t *testing.T) {
	t.Parallel()

	permissions, err := parseMacaroonPermissions([]string{
		"info:read", "uri:/waverpc.DaemonService/GetInfo",
	})
	require.NoError(t, err)
	require.Equal(t, []*waverpc.MacaroonPermission{
		{
			Entity: "info",
			Action: "read",
		},
		{
			Entity: "uri",
			Action: "/waverpc.DaemonService/GetInfo",
		},
	}, permissions)

	for _, arg := range []string{"info", ":read", "info:"} {
		_, err := parseMacaroonPermissions([]string{arg})
		require.Error(t, err, arg)
	}
}

// TestBakeMacaroonCommand verifies the CLI forwards the requested root key
// and scopes, then emits the returned credential as JSON.
func TestBakeMacaroonCommand(t *testing.T) {
	macHex := testMacaroonHex(t)
	var captured *waverpc.BakeMacaroonRequest
	stub := &macaroonClientStub{
		bake: func(
			req *waverpc.BakeMacaroonRequest,
		) *waverpc.BakeMacaroonResponse {

			captured = req

			return &waverpc.BakeMacaroonResponse{
				Macaroon: macHex,
			}
		},
	}
	useMacaroonClientStub(t, stub)

	root := newRootCmd(false)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{
		"bakemacaroon", "--root-key-id", "7", "info:read",
		"uri:/waverpc.DaemonService/GetInfo",
	})
	require.NoError(t, root.Execute())

	require.Equal(t, uint64(7), captured.GetRootKeyId())
	require.Len(t, captured.GetPermissions(), 2)
	expected, err := json.Marshal(map[string]string{
		"macaroon": macHex,
	})
	require.NoError(t, err)
	require.JSONEq(t, string(expected), output.String())
}

// TestBakeMacaroonCommandJSONInput verifies the raw proto request path works
// without positional permission arguments.
func TestBakeMacaroonCommandJSONInput(t *testing.T) {
	macHex := testMacaroonHex(t)
	var captured *waverpc.BakeMacaroonRequest
	stub := &macaroonClientStub{
		bake: func(
			req *waverpc.BakeMacaroonRequest,
		) *waverpc.BakeMacaroonResponse {

			captured = req

			return &waverpc.BakeMacaroonResponse{
				Macaroon: macHex,
			}
		},
	}
	useMacaroonClientStub(t, stub)

	root := newRootCmd(false)
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{
		"bakemacaroon", "--request-json",
		`{"permissions":[{"entity":"info","action":"read"}],` +
			`"rootKeyId":"7"}`,
	})
	require.NoError(t, root.Execute())

	require.Equal(t, uint64(7), captured.GetRootKeyId())
	require.Equal(t, "info", captured.GetPermissions()[0].GetEntity())
}

// TestBakeMacaroonCommandRejectsMixedJSONInput verifies bespoke request fields
// cannot be silently ignored when a raw request is supplied.
func TestBakeMacaroonCommandRejectsMixedJSONInput(t *testing.T) {
	rawRequest := `{"permissions":[{"entity":"info","action":"read"}]}`
	testCases := []struct {
		name string
		args []string
	}{
		{
			name: "positional permission",
			args: []string{
				"bakemacaroon", "info:read", "--request-json",
				rawRequest,
			},
		},
		{
			name: "root key flag",
			args: []string{
				"bakemacaroon", "--root-key-id", "7",
				"--request-json", rawRequest,
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			stub := &macaroonClientStub{
				bake: func(
					*waverpc.BakeMacaroonRequest,
				) *waverpc.BakeMacaroonResponse {

					called = true

					return &waverpc.BakeMacaroonResponse{}
				},
			}
			useMacaroonClientStub(t, stub)

			root := newRootCmd(false)
			root.SetArgs(testCase.args)
			err := root.Execute()
			require.Error(t, err)
			require.False(t, called)
		})
	}
}

// TestBakeMacaroonCommandSavesBinary verifies --save-to writes the decoded
// credential with private permissions and does not repeat it in JSON output.
func TestBakeMacaroonCommandSavesBinary(t *testing.T) {
	macHex := testMacaroonHex(t)
	stub := &macaroonClientStub{
		bake: func(
			*waverpc.BakeMacaroonRequest,
		) *waverpc.BakeMacaroonResponse {

			return &waverpc.BakeMacaroonResponse{
				Macaroon: macHex,
			}
		},
	}
	useMacaroonClientStub(t, stub)

	macaroonPath := filepath.Join(t.TempDir(), "payment.macaroon")
	root := newRootCmd(false)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{
		"bakemacaroon", "--save-to", macaroonPath, "info:read",
	})
	require.NoError(t, root.Execute())

	contents, err := os.ReadFile(macaroonPath)
	require.NoError(t, err)
	expectedContents, err := hex.DecodeString(macHex)
	require.NoError(t, err)
	require.Equal(t, expectedContents, contents)

	info, err := os.Stat(macaroonPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	expected, err := json.Marshal(map[string]string{
		"path": macaroonPath,
	})
	require.NoError(t, err)
	require.JSONEq(
		t, string(expected), output.String(),
	)
}

// TestBakeMacaroonCommandRefusesExistingFile verifies a bad destination is
// rejected before the daemon mints a new credential.
func TestBakeMacaroonCommandRefusesExistingFile(t *testing.T) {
	called := false
	stub := &macaroonClientStub{
		bake: func(
			*waverpc.BakeMacaroonRequest,
		) *waverpc.BakeMacaroonResponse {

			called = true

			return &waverpc.BakeMacaroonResponse{
				Macaroon: testMacaroonHex(t),
			}
		},
	}
	useMacaroonClientStub(t, stub)

	macaroonPath := filepath.Join(t.TempDir(), "payment.macaroon")
	require.NoError(t, os.WriteFile(macaroonPath, []byte("keep"), 0o644))

	root := newRootCmd(false)
	root.SetArgs([]string{
		"bakemacaroon", "--save-to", macaroonPath, "info:read",
	})
	err := root.Execute()
	require.Error(t, err)
	require.False(t, called)

	contents, readErr := os.ReadFile(macaroonPath)
	require.NoError(t, readErr)
	require.Equal(t, []byte("keep"), contents)
}

// TestBakeMacaroonCommandRemovesEmptyOutput verifies a malformed daemon
// response does not leave a credential-shaped file behind.
func TestBakeMacaroonCommandRemovesEmptyOutput(t *testing.T) {
	stub := &macaroonClientStub{
		bake: func(
			*waverpc.BakeMacaroonRequest,
		) *waverpc.BakeMacaroonResponse {

			return &waverpc.BakeMacaroonResponse{}
		},
	}
	useMacaroonClientStub(t, stub)

	macaroonPath := filepath.Join(t.TempDir(), "payment.macaroon")
	root := newRootCmd(false)
	root.SetArgs([]string{
		"bakemacaroon", "--save-to", macaroonPath, "info:read",
	})
	err := root.Execute()
	require.ErrorContains(t, err, "empty macaroon")
	require.NoFileExists(t, macaroonPath)
}

// TestBakeMacaroonCommandAddsExpiry verifies the client can attenuate a
// daemon-baked credential with a standard time-before caveat.
func TestBakeMacaroonCommandAddsExpiry(t *testing.T) {
	stub := &macaroonClientStub{
		bake: func(
			*waverpc.BakeMacaroonRequest,
		) *waverpc.BakeMacaroonResponse {

			return &waverpc.BakeMacaroonResponse{
				Macaroon: testMacaroonHex(t),
			}
		},
	}
	useMacaroonClientStub(t, stub)

	root := newRootCmd(false)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{
		"bakemacaroon", "--valid-for", "60", "info:read",
	})
	require.NoError(t, root.Execute())

	var result bakedMacaroonResult
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	macBytes, err := hex.DecodeString(result.Macaroon)
	require.NoError(t, err)
	rawMacaroon := &macaroon.Macaroon{}
	require.NoError(t, rawMacaroon.UnmarshalBinary(macBytes))
	require.Len(t, rawMacaroon.Caveats(), 1)
	require.True(
		t,
		strings.HasPrefix(
			string(rawMacaroon.Caveats()[0].Id),
			"time-before ",
		),
	)
}

// testMacaroonHex returns a valid macaroon for command output tests.
func testMacaroonHex(t *testing.T) string {
	t.Helper()

	rootKey := bytes.Repeat([]byte{1}, macaroons.RootKeyLen)
	rawMacaroon, err := macaroons.BakeFromRootKey(
		rootKey, []bakery.Op{{
			Entity: "info",
			Action: "read",
		}},
	)
	require.NoError(t, err)

	macBytes, err := rawMacaroon.MarshalBinary()
	require.NoError(t, err)

	return hex.EncodeToString(macBytes)
}

// useMacaroonClientStub installs a test client for one non-parallel test.
func useMacaroonClientStub(t *testing.T, stub waverpc.MacaroonServiceClient) {
	t.Helper()
	previous := getMacaroonClient
	getMacaroonClient = func(*cobra.Command) (waverpc.MacaroonServiceClient,
		*grpc.ClientConn, error) {

		return stub, nil, nil
	}
	t.Cleanup(func() {
		getMacaroonClient = previous
	})
}
