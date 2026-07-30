package devrpc

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDevDescribeDumpsMethodSchema exercises the `--describe` path:
// the command must emit a JSON schema for the method's input fields
// without dialing the daemon. An agent that only wants to learn the
// surface should be able to call --describe and never trigger the
// network.
func TestDevDescribeDumpsMethodSchema(t *testing.T) {
	cmd, cleanup := newTestDevCmd(t, &testDaemonServer{}, &bytes.Buffer{})
	defer cleanup()

	stdout, restore := captureStdout(t)
	defer restore()

	cmd.SetArgs([]string{"daemon", "list-vtxos", "--describe"})
	require.NoError(t, cmd.Execute())

	data := stdout()
	require.NotEmpty(t, data)

	var desc methodDescription
	require.NoError(t, json.Unmarshal(data, &desc))

	require.Equal(t, "ListVTXOs", desc.Method)
	require.Equal(t, "waverpc.DaemonService", desc.Service)
	require.Equal(t, "waverpc.ListVTXOsRequest", desc.RequestType)
	require.Equal(t, "waverpc.ListVTXOsResponse", desc.ResponseType)
	require.False(t, desc.ServerStreaming)

	// status-filter is an enum field — describe should surface
	// the enum value list so an agent doesn't have to grep proto
	// sources for the legal names.
	var statusField *fieldDescription
	for i := range desc.Fields {
		require.NotContains(t, desc.Fields[i].Path, "_")
		if desc.Fields[i].Path == "status-filter" {
			statusField = &desc.Fields[i]

			break
		}
	}
	require.NotNil(t, statusField, "status-filter must appear in schema")
	require.Equal(t, "enum", statusField.Type)
	require.Contains(t, statusField.EnumValues, "VTXO_STATUS_LIVE")
}

// TestDevDescribeRespectsOneofGroup confirms describe records the
// containing oneof for fields that are mutually exclusive — agents
// rely on this signal to know which flag combinations conflict.
func TestDevDescribeRespectsOneofGroup(t *testing.T) {
	cmd, cleanup := newTestDevCmd(t, &testDaemonServer{}, &bytes.Buffer{})
	defer cleanup()

	stdout, restore := captureStdout(t)
	defer restore()

	cmd.SetArgs([]string{"daemon", "prepare-oor", "--describe"})
	require.NoError(t, cmd.Execute())

	var desc methodDescription
	require.NoError(t, json.Unmarshal(stdout(), &desc))

	// The recipient message has a oneof for destination
	// (address / pubkey / pk_script). At least one of those
	// nested fields must carry the oneof group name in the
	// schema.
	var sawOneof bool
	for _, f := range desc.Fields {
		if f.OneofGroup != "" && strings.HasPrefix(
			f.Path, "recipient.",
		) {

			sawOneof = true

			break
		}
	}
	require.True(
		t, sawOneof, "expected at least one recipient.* field to "+
			"carry an oneof_group annotation; got %+v", desc.Fields,
	)
}

// captureStdout redirects os.Stdout into a pipe for the duration of
// the test and returns a getter for the captured bytes plus a
// restoration func. Tests need this because printMethodDescription
// writes directly to os.Stdout (matching the other dev commands),
// not through the cmd's writer.
//
// The getter closes the pipe's write end before draining the read
// goroutine so io.Copy in the reader sees EOF; the deferred restore
// is idempotent so callers can keep their normal defer-restore
// pattern without deadlocking on a double-close.
func captureStdout(t *testing.T) (func() []byte, func()) {
	t.Helper()

	r, w, err := os.Pipe()
	require.NoError(t, err)

	orig := os.Stdout
	os.Stdout = w

	done := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()

	var closed bool
	closeWriter := func() {
		if closed {
			return
		}
		closed = true
		_ = w.Close()
	}

	restore := func() {
		closeWriter()
		os.Stdout = orig
	}

	getter := func() []byte {
		closeWriter()

		return <-done
	}

	return getter, restore
}
