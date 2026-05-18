//go:build itest

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const defaultStateFilename = "current.json"

// lndState captures everything `lncli` needs to talk to a running LND node.
type lndState struct {
	Name          string `json:"name"`
	GRPCAddr      string `json:"grpc_addr"`
	TLSCertPath   string `json:"tls_cert_path"`
	MacaroonPath  string `json:"macaroon_path"`
	DataDir       string `json:"data_dir"`
	ContainerName string `json:"container_name"`
}

// arkClientState captures everything `darepocli` needs to talk to a running
// darepod instance, plus optional boarding-related metadata.
type arkClientState struct {
	Name              string `json:"name"`
	RPCAddr           string `json:"rpc_addr"`
	DataDir           string `json:"data_dir"`
	Wallet            string `json:"wallet"`
	BoardingAddress   string `json:"boarding_address,omitempty"`
	BoardingAmount    int64  `json:"boarding_amount_sat,omitempty"`
	BoardingConfirmed bool   `json:"boarding_confirmed,omitempty"`
}

// harnessState is persisted to disk by `arktest start` so the other
// subcommands (`mine`, `info`, `aliases`) can find the running topology
// without their own arguments.
type harnessState struct {
	StartedAt string `json:"started_at"`

	DataDir      string `json:"datadir"`
	StateFile    string `json:"state_file"`
	ArtifactsDir string `json:"artifacts_dir"`
	RunDir       string `json:"run_dir"`
	BinDir       string `json:"bin_dir"`

	ArkAdminAddr string `json:"ark_admin_addr"`
	ArkRPCAddr   string `json:"ark_rpc_addr"`
	EsploraURL   string `json:"esplora_url"`

	BitcoindRPC           string `json:"bitcoind_rpc"`
	BitcoindRPCUser       string `json:"bitcoind_rpc_user"`
	BitcoindRPCPass       string `json:"bitcoind_rpc_pass"`
	BitcoindZMQBlock      string `json:"bitcoind_zmq_block"`
	BitcoindZMQTx         string `json:"bitcoind_zmq_tx"`
	BitcoindContainerName string `json:"bitcoind_container_name"`

	OperatorLND lndState `json:"operator_lnd"`

	// Clients is keyed by logical client name (e.g. "alice", "bob").
	Clients map[string]*arkClientState `json:"clients"`

	// ClientLNDs holds per-client LND state when a client is using the
	// LND wallet backend. Keyed by the same logical client name as
	// Clients.
	ClientLNDs map[string]*lndState `json:"client_lnds"`
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".arktest"
	}

	return filepath.Join(home, ".arktest")
}

func stateFilePath() string {
	return filepath.Join(dataDir, defaultStateFilename)
}

func saveState(s *harnessState) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("mkdir datadir: %w", err)
	}

	// Only stamp StartedAt on the first save so subsequent
	// subcommands (board, etc.) that round-trip the state file don't
	// overwrite the original harness start time.
	if s.StartedAt == "" {
		s.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	s.DataDir = dataDir
	s.StateFile = stateFilePath()

	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	if err := os.WriteFile(s.StateFile, buf, 0o644); err != nil {
		return fmt.Errorf("write state: %w", err)
	}

	return nil
}

func loadState() (*harnessState, error) {
	path := stateFilePath()

	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w (is `arktest start` "+
			"running?)", path, err)
	}

	var s harnessState
	if err := json.Unmarshal(buf, &s); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}

	return &s, nil
}

func deleteState() error {
	path := stateFilePath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove state: %w", err)
	}

	return nil
}
