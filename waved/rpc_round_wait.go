package waved

import (
	"github.com/lightninglabs/wavelength/round"
	"github.com/lightninglabs/wavelength/waverpc"
	"google.golang.org/protobuf/proto"
)

// populateRoundWait exposes assembly progress without inspecting the mutable
// FSM environment. Correlators attach the wait to the actual pending operation.
func populateRoundWait(info *waverpc.RoundInfo, state round.ClientState) {
	pending, ok := state.(*round.PendingRoundAssembly)
	if !ok {
		return
	}

	info.WaitingReason = "Assembling round inputs"
	for _, boarding := range pending.Boarding {
		info.InputOutpoints = append(
			info.InputOutpoints, boarding.Outpoint.String(),
		)
		if boarding.Address.Address != nil {
			info.BoardingAddresses = append(
				info.BoardingAddresses,
				boarding.Address.Address.EncodeAddress(),
			)
		}
	}
	for _, forfeit := range pending.Forfeits {
		if forfeit.VTXOOutpoint != nil {
			info.InputOutpoints = append(
				info.InputOutpoints,
				forfeit.VTXOOutpoint.String(),
			)
		}
	}

	wait := pending.BatchWait
	if wait.Cutoff.IsZero() {
		return
	}

	info.WaitingReason = "Waiting for scheduled batch registration"
	info.BatchWindow = &waverpc.ScheduledBatchWindow{
		RegistrationOpensUnix: wait.Opens.Unix(),
		CutoffUnix:            wait.Cutoff.Unix(),
		PlannedJoinUnixMs:     wait.PlannedJoin.UnixMilli(),
	}
}

// changedRoundStatuses reports metadata changes even when the FSM state stays
// the same. Temporary round keys keep simultaneous assembly attempts distinct.
func changedRoundStatuses(previous map[string]*waverpc.RoundInfo,
	current []*waverpc.RoundInfo) []*waverpc.RoundInfo {

	var changed []*waverpc.RoundInfo
	seen := make(map[string]struct{}, len(current))
	for _, info := range current {
		key := info.GetRoundKey()
		seen[key] = struct{}{}
		if proto.Equal(previous[key], info) {
			continue
		}
		snapshot := &waverpc.RoundInfo{}
		proto.Merge(snapshot, info)
		previous[key] = snapshot
		changed = append(changed, info)
	}
	for key := range previous {
		if _, ok := seen[key]; !ok {
			delete(previous, key)
		}
	}

	return changed
}
