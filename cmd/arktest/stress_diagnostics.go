//go:build itest

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"sync"
	"time"
)

const (
	stressTraceName        = "trace.out"
	stressCPUProfileName   = "cpu.pprof"
	stressBlockProfileName = "block.pprof"
	stressMutexProfileName = "mutex.pprof"

	stressBlockProfileRate     = 1000
	stressMutexProfileFraction = 100
)

type stressDiagnosticPaths struct {
	TraceFile        string
	CPUProfileFile   string
	BlockProfileFile string
	MutexProfileFile string
}

type stressDiagnostics struct {
	events *eventLog
	paths  stressDiagnosticPaths

	mu             sync.Mutex
	traceFile      *os.File
	traceTimer     *time.Timer
	cpuFile        *os.File
	blockProfile   bool
	mutexProfile   bool
	mutexPrevRate  int
	stopped        bool
	traceStopError error
}

// stressTraceRegion records a named region in active Go runtime traces.
func stressTraceRegion(ctx context.Context, name string, fn func()) {
	trace.WithRegion(ctx, name, fn)
}

// startDiagnostics starts configured runtime diagnostics for the stress run.
func (r *stressRunner) startDiagnostics() {
	diagnostics, err := newStressDiagnostics(
		r.state.RunDir, r.cfg, r.events,
	)
	if err != nil {
		r.t.Fatalf("start diagnostics: %v", err)
	}
	if diagnostics == nil {
		return
	}

	r.diagnostics = diagnostics
	r.diagnosticPaths = diagnostics.paths
}

// stopDiagnostics flushes and closes active stress diagnostic artifacts.
func (r *stressRunner) stopDiagnostics(reason string) {
	if r.diagnostics == nil {
		return
	}

	if err := r.diagnostics.Stop(reason); err != nil {
		r.t.Fatalf("stop diagnostics: %v", err)
	}
}

// newStressDiagnostics allocates and starts configured diagnostic captures.
func newStressDiagnostics(runDir string, cfg stressConfig,
	events *eventLog) (*stressDiagnostics, error) {

	enabled := cfg.trace || cfg.cpuProfile || cfg.blockProfile ||
		cfg.mutexProfile
	if !enabled {
		return nil, nil
	}

	d := &stressDiagnostics{events: events}

	var err error
	if cfg.trace {
		d.paths.TraceFile, err = stressArtifactPath(
			runDir, cfg.traceFile, stressTraceName,
		)
		if err != nil {
			return nil, err
		}
	}
	if cfg.cpuProfile {
		d.paths.CPUProfileFile, err = stressArtifactPath(
			runDir, cfg.cpuProfileFile, stressCPUProfileName,
		)
		if err != nil {
			return nil, err
		}
	}
	if cfg.blockProfile {
		d.paths.BlockProfileFile, err = stressArtifactPath(
			runDir, cfg.blockProfileFile, stressBlockProfileName,
		)
		if err != nil {
			return nil, err
		}
	}
	if cfg.mutexProfile {
		d.paths.MutexProfileFile, err = stressArtifactPath(
			runDir, cfg.mutexProfileFile, stressMutexProfileName,
		)
		if err != nil {
			return nil, err
		}
	}

	if d.paths.TraceFile != "" {
		if err := d.startTrace(cfg.traceDuration); err != nil {
			_ = d.Stop("start_failed")

			return nil, err
		}
	}
	if d.paths.CPUProfileFile != "" {
		if err := d.startCPUProfile(); err != nil {
			_ = d.Stop("start_failed")

			return nil, err
		}
	}
	if d.paths.BlockProfileFile != "" {
		d.blockProfile = true
		runtime.SetBlockProfileRate(stressBlockProfileRate)
	}
	if d.paths.MutexProfileFile != "" {
		d.mutexProfile = true
		d.mutexPrevRate = runtime.SetMutexProfileFraction(
			stressMutexProfileFraction,
		)
	}

	if events != nil {
		events.Printf("diagnostics", map[string]any{
			"trace_file":        d.paths.TraceFile,
			"trace_duration_ms": cfg.traceDuration.Milliseconds(),
			"cpu_profile":       d.paths.CPUProfileFile,
			"block_profile":     d.paths.BlockProfileFile,
			"block_rate_ns":     stressBlockProfileRate,
			"mutex_profile":     d.paths.MutexProfileFile,
			"mutex_fraction":    stressMutexProfileFraction,
		},
			"runtime diagnostics enabled")
	}

	return d, nil
}

// stressArtifactPath resolves a diagnostics path under the run directory.
func stressArtifactPath(runDir, requested, defaultName string) (string, error) {
	path := requested
	if path == "" {
		path = defaultName
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(runDir, path)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create diagnostic dir: %w", err)
	}

	return path, nil
}

// startTrace opens the trace artifact and starts the Go runtime tracer.
func (d *stressDiagnostics) startTrace(duration time.Duration) error {
	f, err := os.Create(d.paths.TraceFile)
	if err != nil {
		return fmt.Errorf("create runtime trace: %w", err)
	}
	d.traceFile = f

	if err := trace.Start(f); err != nil {
		_ = f.Close()
		d.traceFile = nil

		return fmt.Errorf("start runtime trace: %w", err)
	}

	if duration > 0 {
		d.traceTimer = time.AfterFunc(duration, func() {
			if err := d.stopTrace("duration"); err != nil {
				d.mu.Lock()
				d.traceStopError = err
				d.mu.Unlock()
			}
		})
	}

	return nil
}

// startCPUProfile opens the CPU artifact and starts pprof sampling.
func (d *stressDiagnostics) startCPUProfile() error {
	f, err := os.Create(d.paths.CPUProfileFile)
	if err != nil {
		return fmt.Errorf("create CPU profile: %w", err)
	}
	d.cpuFile = f

	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		d.cpuFile = nil

		return fmt.Errorf("start CPU profile: %w", err)
	}

	return nil
}

// Stop flushes all active diagnostic captures and closes their files.
func (d *stressDiagnostics) Stop(reason string) error {
	d.mu.Lock()
	if d.stopped {
		err := d.traceStopError
		d.mu.Unlock()

		return err
	}
	d.stopped = true
	timer := d.traceTimer
	d.traceTimer = nil
	d.mu.Unlock()

	if timer != nil {
		timer.Stop()
	}

	var firstErr error
	if err := d.stopTrace(reason); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := d.stopCPUProfile(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := d.writeRuntimeProfile(
		"block", d.paths.BlockProfileFile,
	); err != nil && firstErr == nil {

		firstErr = err
	}
	if err := d.writeRuntimeProfile(
		"mutex", d.paths.MutexProfileFile,
	); err != nil && firstErr == nil {

		firstErr = err
	}

	if d.blockProfile {
		runtime.SetBlockProfileRate(0)
	}
	if d.mutexProfile {
		runtime.SetMutexProfileFraction(d.mutexPrevRate)
	}

	d.mu.Lock()
	if d.traceStopError != nil && firstErr == nil {
		firstErr = d.traceStopError
	}
	d.mu.Unlock()

	if d.events != nil {
		fields := map[string]any{
			"reason":        reason,
			"trace_file":    d.paths.TraceFile,
			"cpu_profile":   d.paths.CPUProfileFile,
			"block_profile": d.paths.BlockProfileFile,
			"mutex_profile": d.paths.MutexProfileFile,
		}
		if firstErr != nil {
			fields["error"] = firstErr.Error()
			d.events.Printf("diagnostics_failed", fields,
				"runtime diagnostics stop failed err=%v",
				firstErr)
		} else {
			d.events.Printf("diagnostics", fields,
				"runtime diagnostics stopped reason=%s", reason)
		}
	}

	return firstErr
}

// stopTrace stops the Go runtime tracer and closes the trace artifact.
func (d *stressDiagnostics) stopTrace(reason string) error {
	d.mu.Lock()
	f := d.traceFile
	if f == nil {
		d.mu.Unlock()

		return nil
	}
	d.traceFile = nil
	d.mu.Unlock()

	trace.Stop()
	if err := f.Close(); err != nil {
		return fmt.Errorf("close runtime trace: %w", err)
	}

	if d.events != nil {
		d.events.Printf("diagnostics", map[string]any{
			"reason": reason,
			"path":   d.paths.TraceFile,
		},
			"runtime trace stopped reason=%s", reason)
	}

	return nil
}

// stopCPUProfile stops pprof CPU sampling and closes the profile artifact.
func (d *stressDiagnostics) stopCPUProfile() error {
	f := d.cpuFile
	if f == nil {
		return nil
	}
	d.cpuFile = nil

	pprof.StopCPUProfile()

	if err := f.Close(); err != nil {
		return fmt.Errorf("close CPU profile: %w", err)
	}

	return nil
}

// writeRuntimeProfile snapshots a named runtime profile to path.
func (d *stressDiagnostics) writeRuntimeProfile(name, path string) error {
	if path == "" {
		return nil
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s profile: %w", name, err)
	}
	defer func() {
		_ = f.Close()
	}()

	profile := pprof.Lookup(name)
	if profile == nil {
		return fmt.Errorf("%s profile unavailable", name)
	}
	if err := profile.WriteTo(f, 0); err != nil {
		return fmt.Errorf("write %s profile: %w", name, err)
	}

	return nil
}
