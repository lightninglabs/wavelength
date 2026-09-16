package waveclicommands

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/lightninglabs/wavelength/rpc/roundpb"
	"github.com/spf13/cobra"
)

// addServiceFlags exposes explicit authorization without changing legacy calls.
func addServiceFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("service", "", "execution service: scheduled or immediate")
	f.String(
		"operation-id", "",
		"stable random operation ID (64 hex characters)",
	)
	f.Uint64("schedule-version", 0, "published schedule version")
	f.Uint64("slot-index", 0, "published slot index")
	f.String("operation-expiry", "", "absolute operation expiry (RFC3339)")
	f.Uint64(
		"service-fee-limit-sat", 0,
		"maximum operator fee; zero permits only free execution",
	)
	f.Bool(
		"allow-immediate-fallback", false,
		"authorize immediate execution after safe scheduled failure",
	)
}

// serviceFromFlags requires a reusable identity and explicit fee and lifetime
// bounds. Callers can also supply the same fields through the JSON request.
func serviceFromFlags(cmd *cobra.Command) (*roundpb.ServiceRequest, error) {
	f := cmd.Flags()
	mode, _ := f.GetString("service")
	if mode == "" {
		for _, name := range []string{
			"operation-id", "schedule-version", "slot-index",
			"operation-expiry", "service-fee-limit-sat",
			"allow-immediate-fallback",
		} {
			if f.Changed(name) {
				return nil, fmt.Errorf("--%s requires "+
					"--service", name)
			}
		}

		return nil, nil
	}
	if !f.Changed("service-fee-limit-sat") {
		return nil, fmt.Errorf("--service requires " +
			"--service-fee-limit-sat")
	}
	if flag := f.Lookup("no-join"); flag != nil &&
		flag.Value.String() == "true" {
		return nil, fmt.Errorf("--service cannot be combined with " +
			"--no-join")
	}
	id, _ := f.GetString("operation-id")
	rawID, err := hex.DecodeString(id)
	if err != nil || len(rawID) != 32 {
		return nil, fmt.Errorf("--operation-id must be 64 hex " +
			"characters")
	}
	expiryText, _ := f.GetString("operation-expiry")
	expiry, err := time.Parse(time.RFC3339, expiryText)
	if err != nil || expiry.Unix() <= 0 {
		return nil, fmt.Errorf("--operation-expiry must be an " +
			"RFC3339 time after 1970")
	}
	request := &roundpb.ServiceRequest{
		OperationId: rawID, ExpiresAtUnix: uint64(expiry.Unix()),
	}
	switch mode {
	case "scheduled":
		request.Mode = roundpb.ServiceMode_SERVICE_SCHEDULED

	case "immediate":
		request.Mode = roundpb.ServiceMode_SERVICE_IMMEDIATE

	default:
		return nil, fmt.Errorf("--service must be scheduled or " +
			"immediate")
	}
	request.ScheduleVersion, _ = f.GetUint64("schedule-version")
	request.SlotIndex, _ = f.GetUint64("slot-index")
	request.FeeLimitSat, _ = f.GetUint64("service-fee-limit-sat")
	request.AllowFallback, _ = f.GetBool("allow-immediate-fallback")
	if _, err := roundpb.ServiceRequestFromProto(request); err != nil {
		return nil, err
	}

	return request, nil
}

// serviceSchemaParams describes the explicit CLI authorization fields.
func serviceSchemaParams() []schemaParam {
	return []schemaParam{
		{Name: "service", Type: "string",
			Description: "scheduled or immediate execution"},
		{Name: "operation-id", Type: "string",
			Description: "stable operation ID (64 hex characters)"},
		{Name: "schedule-version", Type: "uint64",
			Description: "published schedule version"},
		{Name: "slot-index", Type: "uint64",
			Description: "published slot index"},
		{Name: "operation-expiry", Type: "string",
			Description: "absolute operation expiry (RFC3339)"},
		{Name: "service-fee-limit-sat", Type: "uint64",
			Description: "fee cap in sats; zero permits free only"},
		{Name: "allow-immediate-fallback", Type: "bool",
			Description: "authorize fallback after safe failure"},
	}
}
