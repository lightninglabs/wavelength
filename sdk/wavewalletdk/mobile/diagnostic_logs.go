//go:build mobile && wavewalletrpc && swapruntime

package mobile

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lightninglabs/wavelength/sdk/wavewalletdk"
)

// diagnosticLogEntry is a bounded public log record, never a wallet secret.
//
//nolint:tagliatelle // Preserve the published gomobile PascalCase keys.
type diagnosticLogEntry struct {
	ID      string `json:"ID"`
	Time    int64  `json:"Time"`
	Level   string `json:"Level"`
	Source  string `json:"Source"`
	Message string `json:"Message"`
}

// diagnosticLogBuffer owns one wallet's bounded log history. Writes never
// call Swift or wait on UI rendering. Files rotate at one MiB; memory retains
// at most 2,000 records, each capped at 4 KiB.
type diagnosticLogBuffer struct {
	mu        sync.Mutex
	entries   []diagnosticLogEntry
	path      string
	size      int64
	fileError bool
}

// diagnosticPrefix extracts the daemon's established level/subsystem prefix.
var diagnosticPrefix = regexp.MustCompile(
	`\[(TRC|DBG|INF|WRN|ERR|CRT)\]\s+([A-Z0-9]+)`,
)

// attachDiagnosticLogs runs only inside an admitted mobile startup. The
// writer belongs to that daemon, while reads use a separate buffer lock.
func attachDiagnosticLogs(cfg *wavewalletdk.Config, enabled bool) {
	if !enabled {
		state.mu.Lock()
		state.logs = nil
		state.mu.Unlock()

		return
	}
	logs := newDiagnosticLogBuffer(
		filepath.Join(
			cfg.DataDir, "diagnostics", cfg.Network, "native.jsonl",
		),
	)
	state.mu.Lock()
	state.logs = logs
	state.mu.Unlock()
	cfg.LogWriter = io.MultiWriter(logs, os.Stdout)
}

// diagnosticLoggingRequested reads only the already-validated capture opt-in.
// External-seed startup owns the nested config; ordinary Start owns the root.
func diagnosticLoggingRequested(data []byte, nested bool) bool {
	if nested {
		var envelope struct {
			Config json.RawMessage `json:"config"`
		}
		if json.Unmarshal(data, &envelope) != nil {
			return false
		}
		data = envelope.Config
	}
	var config struct {
		Enabled bool `json:"diagnostic_logs"`
	}

	return json.Unmarshal(data, &config) == nil && config.Enabled
}

// newDiagnosticLogBuffer loads only the bounded current and rotated logs.
// A storage failure leaves memory capture usable and never prevents startup.
func newDiagnosticLogBuffer(path string) *diagnosticLogBuffer {
	logs := &diagnosticLogBuffer{
		path: path, entries: make([]diagnosticLogEntry, 0),
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		logs.fileError = true

		return logs
	}
	for _, name := range []string{path + ".1", path} {
		//nolint:gosec // G304: trusted host rotation paths.
		file, err := os.Open(name)
		if err != nil {
			continue
		}
		scan := bufio.NewScanner(io.LimitReader(file, 2<<20))
		scan.Buffer(make([]byte, 8192), 32768)
		for scan.Scan() {
			var entry diagnosticLogEntry
			if json.Unmarshal(scan.Bytes(), &entry) == nil {
				logs.entries = append(logs.entries, entry)
			}
		}
		_ = file.Close()
	}
	if len(logs.entries) > 2000 {
		logs.entries = logs.entries[len(logs.entries)-2000:]
	}
	if stat, err := os.Stat(path); err == nil {
		logs.size = stat.Size()
	}

	return logs
}

// Write captures complete logger writes without forwarding secrets to the
// app-visible history. Oversized messages are truncated before persistence.
func (b *diagnosticLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Bound processing as well as retained messages. One malformed logger
	// write must not produce an unbounded number of records or file writes.
	input := p[:min(len(p), 8192)]
	for index, line := range strings.SplitN(string(input), "\n", 33) {
		if index == 32 {
			break
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		entry := diagnosticLogEntry{
			ID: time.Now().UTC().Format(
				"20060102T150405.000000000",
			),
			Time: time.Now().UnixMilli(), Level: "INFO",
			Source: "Wavelength", Message: line,
		}
		if match := diagnosticPrefix.FindStringSubmatch(
			line,
		); match != nil {

			entry.Level = map[string]string{
				"TRC": "TRACE", "DBG": "DEBUG", "INF": "INFO",
				"WRN": "WARN", "ERR": "ERROR", "CRT": "ERROR",
			}[match[1]]
			// A small source substring must not retain the entire
			// write.
			entry.Source = strings.Clone(
				match[2][:min(len(match[2]), 64)],
			)
			location := diagnosticPrefix.FindStringIndex(line)
			entry.Message = strings.TrimSpace(
				strings.TrimPrefix(
					line[location[1]:], ":",
				),
			)
		}
		lower := strings.ToLower(line)
		for _, secret := range []string{
			"password", "mnemonic", "seed_entropy", "preimage",
			"macaroon", "authorization", "feed_token", "feedtoken",
			"private_key", "privatekey", "wake_capability",
			"alert_capability", "bearer ", "seed",
			"secret", "token=",
		} {
			if strings.Contains(lower, secret) {
				entry.Message = "[Sensitive log entry omitted]"
				break
			}
		}
		if len(entry.Message) > 4096 {
			entry.Message = entry.Message[:4096] + "…"
		}
		// Substrings must not retain an oversized original logger
		// write.
		entry.Message = strings.Clone(entry.Message)
		b.entries = append(b.entries, entry)
		if len(b.entries) > 2000 {
			copy(b.entries, b.entries[len(b.entries)-2000:])
			b.entries = b.entries[:2000]
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		encoded = append(encoded, '\n')
		if b.size+int64(len(encoded)) > 1<<20 {
			_ = os.Remove(b.path + ".1")
			if err := os.Rename(b.path, b.path+".1"); err != nil {
				b.fileError = true
				continue
			}
			b.size = 0
		}
		file, err := os.OpenFile(
			b.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600,
		)
		if err != nil {
			b.fileError = true
			continue
		}
		written, err := file.Write(encoded)
		b.size += int64(written)
		b.fileError = err != nil
		_ = file.Close()
	}

	return len(p), nil
}

// DiagnosticLogs returns the active wallet's recent native log records.
// It does not start a daemon, change logging levels or enumerate system logs.
func DiagnosticLogs() ([]byte, error) {
	if _, _, err := activeClient(); err != nil {
		return nil, err
	}
	state.mu.Lock()
	logs := state.logs
	state.mu.Unlock()
	if logs == nil {
		return marshal(struct {
			Entries            []diagnosticLogEntry
			HistoryUnavailable bool
		}{Entries: []diagnosticLogEntry{}, HistoryUnavailable: true})
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()

	return marshal(struct {
		Entries            []diagnosticLogEntry
		HistoryUnavailable bool
	}{Entries: logs.entries, HistoryUnavailable: logs.fileError})
}
