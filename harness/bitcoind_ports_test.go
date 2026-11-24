//go:build itest

package harness

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBitcoindPublishedPorts checks that the host can reach regtest RPC
// without also publishing ports for the image's other Bitcoin networks.
func TestBitcoindPublishedPorts(t *testing.T) {
	h := NewHarness(t, nil)
	h.setupArtifactsAndLogging()
	h.setupDockerEnvironment()
	t.Cleanup(h.Stop)
	h.createDataDirectories()
	h.startBitcoind()

	container, err := h.pool.Client.InspectContainer(
		h.bitcoind.Container.ID,
	)
	require.NoError(t, err)

	var published []string
	for port, bindings := range container.NetworkSettings.Ports {
		if len(bindings) != 0 {
			published = append(published, string(port))
		}
	}
	require.ElementsMatch(t, []string{
		"18443/tcp", "18444/tcp", "28332/tcp", "28333/tcp",
	}, published)

	client, err := h.BitcoinRPCClient()
	require.NoError(t, err)
	t.Cleanup(client.Shutdown)
	_, err = client.GetBlockCount()
	require.NoError(t, err)
}
