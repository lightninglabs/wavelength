package harness

import (
	"errors"
	"testing"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
)

// TestIsDockerPortBindError verifies we recognize common Docker errors that
// indicate a published host port was already in use.
func TestIsDockerPortBindError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "bind already in use",
			err: errors.New(
				"listen tcp4 127.0.0.1:33590: " +
					"bind: address already in use",
			),
			want: true,
		},
		{
			name: "port is already allocated",
			err: errors.New(
				"Bind for 0.0.0.0:8080 failed: " +
					"port is already allocated",
			),
			want: true,
		},
		{
			name: "ports are not available",
			err: errors.New(
				"Ports are not available: exposing port TCP " +
					"0.0.0.0:5432 -> 0.0.0.0:0: " +
					"listen tcp 0.0.0.0:5432: " +
					"bind: address already in use",
			),
			want: true,
		},
		{
			name: "other error",
			err:  errors.New("some other docker error"),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isDockerPortBindError(tc.err))
		})
	}
}

// TestRunWithPortBindRetry verifies we retry transient Docker port bind
// conflicts but do not retry for unrelated errors.
func TestRunWithPortBindRetry(t *testing.T) {
	t.Parallel()

	t.Run("retries on bind errors", func(t *testing.T) {
		var attempts int
		h := &Harness{
			T:    t,
			opts: &Options{},
		}

		res, err := h.runWithPortBindRetry("test-container", func() (
			*dockertest.Resource, error) {

			attempts++
			if attempts < 3 {
				return nil, errors.New("listen tcp4 " +
					"127.0.0.1:12345: bind: address " +
					"already in use")
			}

			return &dockertest.Resource{
				Container: &docker.Container{
					Name: "ok",
				},
			}, nil
		})
		require.NoError(t, err)
		require.NotNil(t, res)
		require.Equal(t, 3, attempts)
	})

	t.Run("does not retry other errors", func(t *testing.T) {
		var attempts int
		h := &Harness{
			T:    t,
			opts: &Options{},
		}

		_, err := h.runWithPortBindRetry("test-container", func() (
			*dockertest.Resource, error) {

			attempts++

			return nil, errors.New("boom")
		})
		require.Error(t, err)
		require.Equal(t, 1, attempts)
	})
}

// TestContainerHasExactName asserts stale-container cleanup does not treat
// Docker's substring name-filter matches as exact container-name matches.
func TestContainerHasExactName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		container docker.APIContainers
		wantName  string
		want      bool
	}{
		{
			name: "exact name",
			container: docker.APIContainers{
				Names: []string{
					"/bitcoin-TestFoo",
				},
			},
			wantName: "bitcoin-TestFoo",
			want:     true,
		},
		{
			name: "exact name with leading slash",
			container: docker.APIContainers{
				Names: []string{
					"/bitcoin-TestFoo",
				},
			},
			wantName: "/bitcoin-TestFoo",
			want:     true,
		},
		{
			name: "substring sibling",
			container: docker.APIContainers{
				Names: []string{
					"/bitcoin-TestFooSubsequentRounds",
				},
			},
			wantName: "bitcoin-TestFoo",
			want:     false,
		},
		{
			name: "multiple names include exact",
			container: docker.APIContainers{
				Names: []string{
					"/alias",
					"/bitcoin-TestFoo",
				},
			},
			wantName: "bitcoin-TestFoo",
			want:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(
				t, tc.want, containerHasExactName(
					tc.container, tc.wantName,
				),
			)
		})
	}
}

// TestContainerNameIncludesHarnessSuffix verifies explicit group names still
// get unique Docker resource names.
func TestContainerNameIncludesHarnessSuffix(t *testing.T) {
	t.Parallel()

	h := &Harness{
		group:            "TestFoo",
		dockerNameSuffix: "abc12345",
	}

	require.Equal(t, "bitcoin-TestFoo-abc12345", h.containerName("bitcoin"))

	h.group = "TestFraudResponseSpentVTXOCheckpointTimeoutSweep"
	h.dockerNameSuffix = "l4zy091q"

	name := h.containerName("bitcoin")
	require.LessOrEqual(t, len(name), maxDockerDNSLabelLen)
	require.Contains(t, name, "-l4zy091q")
}
