package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ory/dockertest/v3"
	"github.com/ory/dockertest/v3/docker"
	"github.com/stretchr/testify/require"
)

const (
	tapdBuildHelperModeEnv   = "WAVELENGTH_TAPD_BUILD_HELPER_MODE"
	tapdBuildHelperBinaryEnv = "WAVELENGTH_TAPD_BUILD_HELPER_BINARY"
	tapdBuildHelperMarkerEnv = "WAVELENGTH_TAPD_BUILD_HELPER_MARKER"
)

// TestNewTapdImageSpec verifies a released image remains the default and a
// local source path selects a harness-scoped image build.
func TestNewTapdImageSpec(t *testing.T) {
	t.Parallel()

	t.Run("released image", func(t *testing.T) {
		const image = "mirror.gcr.io/lightninglabs/" +
			"taproot-assets:v0.8.0"

		spec, err := newTapdImageSpec(&Options{
			TapdImage: image,
		}, "unused")
		require.NoError(t, err)
		require.False(t, spec.local())
		require.Equal(
			t, "mirror.gcr.io/lightninglabs/taproot-assets",
			spec.repository,
		)
		require.Equal(t, "v0.8.0", spec.tag)
		require.Equal(t, image, spec.reference())
	})

	t.Run("local source", func(t *testing.T) {
		buildPath := t.TempDir()
		err := os.WriteFile(
			filepath.Join(buildPath, "dev.Dockerfile"),
			[]byte("FROM scratch\n"), 0o600,
		)
		require.NoError(t, err)

		spec, err := newTapdImageSpec(&Options{
			TapdImage:     "ignored:v1",
			TapdBuildPath: buildPath,
		}, "abc12345")
		require.NoError(t, err)
		require.True(t, spec.local())
		require.Equal(t, localTapdImageRepository, spec.repository)
		require.Equal(t, "abc12345", spec.tag)
		require.Equal(t, buildPath, spec.buildPath)
		require.Equal(t, "dev.Dockerfile", spec.dockerfile)
		require.Equal(
			t, "wavelength-tapd-local:abc12345", spec.reference(),
		)
	})
}

// TestNewTapdImageSpecRequiresDevDockerfile verifies invalid local source
// paths fail before the harness contacts Docker.
func TestNewTapdImageSpecRequiresDevDockerfile(t *testing.T) {
	t.Parallel()

	_, err := newTapdImageSpec(&Options{
		TapdBuildPath: t.TempDir(),
	}, "abc12345")
	require.ErrorContains(t, err, "stat tapd dev.Dockerfile")
}

// TestTapdImageBuildCommand verifies the Docker CLI receives the exact local
// context, Dockerfile, and harness-scoped image tag. It also proves stdout and
// stderr are both routed to the supplied build log.
func TestTapdImageBuildCommand(t *testing.T) {
	buildPath, marker := configureTapdBuildHelper(t, "success")
	spec := tapdImageSpec{
		repository: localTapdImageRepository,
		tag:        "abc12345",
		buildPath:  buildPath,
		dockerfile: "dev.Dockerfile",
	}

	var output bytes.Buffer
	err := runTapdImageBuild(
		t.Context(), spec, spec.reference(), &output,
	)
	require.NoError(t, err)
	require.Contains(t, output.String(), "cwd="+buildPath)
	require.Contains(
		t, output.String(),
		"args=build|--file|dev.Dockerfile|--tag|wavelength-tapd-loca"+
			"l:abc12345|.",
	)
	require.Contains(t, output.String(), "helper stderr")
	require.Equal(t, "invoked\n", readTestFile(t, marker))
}

// TestTapdImageBuildCancellation proves cancellation terminates the Docker CLI
// promptly and returns the context error instead of only "signal: killed".
func TestTapdImageBuildCancellation(t *testing.T) {
	buildPath, _ := configureTapdBuildHelper(t, "block")
	spec := tapdImageSpec{
		repository: localTapdImageRepository,
		tag:        "abc12345",
		buildPath:  buildPath,
		dockerfile: "dev.Dockerfile",
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := runTapdImageBuild(ctx, spec, spec.reference(), io.Discard)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 2*time.Second)
}

// TestTapdImageBuildIsReused proves selection, validation, and building are a
// single harness lifecycle event. Later option or source-tree changes cannot
// cause another tapd instance to use a different image.
func TestTapdImageBuildIsReused(t *testing.T) {
	buildPath, marker := configureTapdBuildHelper(t, "success")
	opts := &Options{
		TapdBuildPath:    buildPath,
		ArtifactsBaseDir: t.TempDir(),
	}
	h := NewHarness(t, opts)
	h.dockerNameSuffix = "abc12345"
	logPath := filepath.Join(t.TempDir(), "harness.log")
	var err error
	h.harnessLogFile, err = os.Create(logPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, h.harnessLogFile.Close())
	})

	repository, tag, err := h.tapdImage()
	require.NoError(t, err)
	require.Equal(t, localTapdImageRepository, repository)
	require.Equal(t, "abc12345", tag)
	require.Equal(
		t, "wavelength-tapd-local:abc12345", h.localTapdImage,
	)

	require.NoError(
		t,
		os.Remove(
			filepath.Join(buildPath, "dev.Dockerfile"),
		),
	)
	opts.TapdBuildPath = t.TempDir()
	opts.TapdImage = "changed.invalid/tapd:v2"

	reusedRepository, reusedTag, err := h.tapdImage()
	require.NoError(t, err)
	require.Equal(t, repository, reusedRepository)
	require.Equal(t, tag, reusedTag)
	require.Equal(t, "invoked\n", readTestFile(t, marker))
	require.NoError(t, h.harnessLogFile.Sync())
	require.Contains(t, readTestFile(t, logPath), "helper stderr")
}

// TestTapdImageBuildFailureIsCached proves a failing build is not silently
// retried and its persisted output location is included in the returned error.
func TestTapdImageBuildFailureIsCached(t *testing.T) {
	buildPath, marker := configureTapdBuildHelper(t, "failure")
	opts := &Options{
		TapdBuildPath:    buildPath,
		ArtifactsBaseDir: t.TempDir(),
	}
	h := NewHarness(t, opts)
	h.dockerNameSuffix = "abc12345"
	logPath := filepath.Join(t.TempDir(), "harness.log")
	var err error
	h.harnessLogFile, err = os.Create(logPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, h.harnessLogFile.Close())
	})

	_, _, firstErr := h.tapdImage()
	require.ErrorContains(t, firstErr, "exit status 17")
	require.ErrorContains(t, firstErr, "output in "+logPath)
	_, _, secondErr := h.tapdImage()
	require.EqualError(t, secondErr, firstErr.Error())
	require.Equal(t, "invoked\n", readTestFile(t, marker))
	require.Empty(t, h.localTapdImage)
	require.NoError(t, h.harnessLogFile.Sync())
	require.Contains(t, readTestFile(t, logPath), "helper stderr")
}

// TestRemoveLocalTapdImage verifies successful builds are cleaned up by exact
// harness-scoped reference with forced image removal.
func TestRemoveLocalTapdImage(t *testing.T) {
	var (
		method string
		path   string
		force  string
	)
	client, err := docker.NewClient("http://docker.invalid")
	require.NoError(t, err)
	client.HTTPClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response,
			error) {

			method = r.Method
			path = r.URL.Path
			force = r.URL.Query().Get("force")

			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(
					strings.NewReader(""),
				),
				Header: make(http.Header),
			}, nil
		}),
	}

	h := &Harness{
		T: t,
		opts: &Options{
			ArtifactsBaseDir: t.TempDir(),
		},
		pool: &dockertest.Pool{
			Client: client,
		},
		localTapdImage: "wavelength-tapd-local:abc12345",
	}
	h.removeLocalTapdImage()

	require.Equal(t, http.MethodDelete, method)
	require.Equal(
		t, "/images/wavelength-tapd-local:abc12345", path,
	)
	require.Equal(t, "1", force)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response,
	error) {

	return f(request)
}

// configureTapdBuildHelper installs a fake docker binary ahead of the real
// PATH. The wrapper execs this test binary so CommandContext cancellation
// targets the long-running helper directly rather than leaving a shell child.
func configureTapdBuildHelper(t *testing.T, mode string) (string, string) {
	t.Helper()

	binary, err := os.Executable()
	require.NoError(t, err)
	binDir := t.TempDir()
	dockerPath := filepath.Join(binDir, "docker")
	script := "#!/bin/sh\nexec \"$" + tapdBuildHelperBinaryEnv +
		"\" -test.run=^TestTapdImageBuildHelperProcess$ -- \"$@\"\n"
	require.NoError(
		t, os.WriteFile(dockerPath, []byte(script), 0o700),
	)

	buildPath := t.TempDir()
	require.NoError(
		t,
		os.WriteFile(
			filepath.Join(buildPath, "dev.Dockerfile"),
			[]byte("FROM scratch\n"), 0o600,
		),
	)
	marker := filepath.Join(t.TempDir(), "invocations")
	t.Setenv(
		"PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	t.Setenv(tapdBuildHelperBinaryEnv, binary)
	t.Setenv(tapdBuildHelperModeEnv, mode)
	t.Setenv(tapdBuildHelperMarkerEnv, marker)

	return buildPath, marker
}

// TestTapdImageBuildHelperProcess is executed in a subprocess through the fake
// docker wrapper installed by configureTapdBuildHelper.
func TestTapdImageBuildHelperProcess(t *testing.T) {
	mode := os.Getenv(tapdBuildHelperModeEnv)
	if mode == "" {
		return
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	separator := 0
	for idx := range os.Args {
		if os.Args[idx] == "--" {
			separator = idx + 1
			break
		}
	}
	fmt.Fprintln(os.Stdout, "cwd="+cwd)
	fmt.Fprintln(
		os.Stdout, "args="+strings.Join(os.Args[separator:], "|"),
	)
	fmt.Fprintln(os.Stderr, "helper stderr")

	marker := os.Getenv(tapdBuildHelperMarkerEnv)
	file, err := os.OpenFile(
		marker, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := file.WriteString("invoked\n"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := file.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	switch mode {
	case "success":
		return

	case "failure":
		os.Exit(17)

	case "block":
		time.Sleep(time.Minute)
		os.Exit(0)

	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode: "+mode)
		os.Exit(2)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(content)
}
