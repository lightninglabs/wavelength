package waveclicommands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lightninglabs/wavelength/rpc/wavewalletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// TestWriteExitExport preserves the portable schema through both output paths
// and never overwrites an existing file or follows a symlink.
func TestWriteExitExport(t *testing.T) {
	t.Parallel()
	response := &wavewalletrpc.ExportExitResponse{
		FormatVersion: 2,
		Network:       "regtest",
		FinalSweep: &wavewalletrpc.ExitFinalSweep{
			RawTxHex: "0304", DestinationAddress: "destination",
		},
		Transactions: []*wavewalletrpc.ExitAncestryTransaction{{
			Txid: "transaction", RawTxHex: "0102", Kind: "tree",
		}},
	}
	cmd := newExitExportCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	require.NoError(t, writeExitExport(cmd, "", response))
	decoded := &wavewalletrpc.ExportExitResponse{}
	require.NoError(t, protojson.Unmarshal(stdout.Bytes(), decoded))
	require.True(t, proto.Equal(response, decoded))
	path := filepath.Join(t.TempDir(), "ancestry.json")
	require.NoError(t, writeExitExport(cmd, path, response))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, stdout.Bytes(), data)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.ErrorIs(t, writeExitExport(cmd, path, response), os.ErrExist)
	link := filepath.Join(t.TempDir(), "link.json")
	require.NoError(t, os.Symlink(path, link))
	require.ErrorIs(t, writeExitExport(cmd, link, response), os.ErrExist)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, after)
}

// TestExitExportRejectsInvalidOutpoint validates before dialing the daemon.
func TestExitExportRejectsInvalidOutpoint(t *testing.T) {
	t.Parallel()
	cmd := newExitExportCmd()
	cmd.SetArgs([]string{"--outpoint", "invalid"})
	require.Error(t, cmd.Execute())
}

// TestExitExportRejectsInvalidSweepAddress validates before dialing the daemon.
func TestExitExportRejectsInvalidSweepAddress(t *testing.T) {
	t.Parallel()
	cmd := newExitExportCmd()
	cmd.SetArgs([]string{
		"--outpoint", strings.Repeat("0", 63) + "1:0",
		"--sweep-address", "address\n",
	})
	require.Error(t, cmd.Execute())
}
