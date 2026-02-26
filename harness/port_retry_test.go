package harness

import (
	"errors"
	"testing"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
)

const dockerImagePullErr = "error pulling image configuration: " +
	"download failed after attempts=1: unknown blob"

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

// TestIsDockerImagePullError verifies we detect transient image pull failures
// that should be retried.
func TestIsDockerImagePullError(t *testing.T) {
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
			name: "unknown blob",
			err:  errors.New(dockerImagePullErr),
			want: true,
		},
		{
			name: "rate limited",
			err: errors.New(
				"toomanyrequests: too many requests",
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
			got := isDockerImagePullError(tc.err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestIsDockerStartTimeoutError verifies timeout error detection for docker
// start attempts.
func TestIsDockerStartTimeoutError(t *testing.T) {
	t.Parallel()

	require.True(t, isDockerStartTimeoutError(
		errors.New("docker start timed out for bitcoin-test"),
	))
	require.False(t, isDockerStartTimeoutError(
		errors.New("some other error"),
	))
	require.False(t, isDockerStartTimeoutError(nil))
}

// TestMirrorFallbackImage verifies mirror image rewriting behavior.
func TestMirrorFallbackImage(t *testing.T) {
	t.Parallel()

	fallback, ok := mirrorFallbackImage(
		"mirror.gcr.io/lightninglabs/bitcoin-core:29",
	)
	require.True(t, ok)
	require.Equal(t, "lightninglabs/bitcoin-core:29", fallback)

	fallback, ok = mirrorFallbackImage("lightninglabs/bitcoin-core:29")
	require.False(t, ok)
	require.Empty(t, fallback)
}

// TestRunWithPortBindRetry verifies we retry transient Docker startup failures
// but do not retry for unrelated errors.
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
				return nil, errors.New(
					"listen tcp4 127.0.0.1:12345: bind: " +
						"address already in use",
				)
			}

			return &dockertest.Resource{
				Container: &docker.Container{Name: "ok"},
			}, nil
		})
		require.NoError(t, err)
		require.NotNil(t, res)
		require.Equal(t, 3, attempts)
	})

	t.Run("retries on image pull errors", func(t *testing.T) {
		var attempts int
		h := &Harness{
			T:    t,
			opts: &Options{},
		}

		res, err := h.runWithPortBindRetry("test-container", func() (
			*dockertest.Resource, error) {

			attempts++
			if attempts < 3 {
				return nil, errors.New(dockerImagePullErr)
			}

			return &dockertest.Resource{
				Container: &docker.Container{Name: "ok"},
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
