package round

import (
	"fmt"

	"github.com/lightninglabs/wavelength/lib/actormsg"
)

// newDurableLocalCommand encodes wallet requests before they derive round keys.
func newDurableLocalCommand(msg actormsg.RoundReceivable) (
	*durableClientCommand, error) {

	m := &durableClientCommand{}
	var err error
	switch req := msg.(type) {
	case *GetClientStateRequest:
		m.Kind = clientCommandGetState

	case *RegisterVTXORequestsRequest:
		m.Kind = clientCommandOutputs
		m.Payload, err = encodeDurableOutputRequest(req)

	case *RefreshVTXORequest:
		m.Kind = clientCommandRefresh
		m.Payload, err = encodeDurableRefresh(req)

	case *RefreshVTXOCohortRequest:
		m.Kind = clientCommandCohort
		m.Payload, err = encodeDurableList(
			req.Requests, encodeDurableRefresh,
		)

	case *actormsg.TriggerBoardMsg:
		m.Kind = clientCommandTrigger
		m.Payload, err = encodeDurableTrigger(req)

	default:
		return nil, fmt.Errorf("unsupported durable client command %T",
			msg)
	}
	if err != nil {
		return nil, err
	}

	return m, nil
}

// localMessage restores one complete wallet request for ordinary dispatch.
func (m *durableClientCommand) localMessage() (actormsg.RoundReceivable,
	error) {

	switch m.Kind {
	case clientCommandGetState:
		return &GetClientStateRequest{}, nil

	case clientCommandOutputs:
		return decodeDurableOutputRequest(m.Payload)

	case clientCommandRefresh:
		return decodeDurableRefresh(m.Payload)

	case clientCommandCohort:
		requests, err := decodeDurableList(
			m.Payload, decodeDurableRefresh,
		)
		if err != nil {
			return nil, err
		}

		return &RefreshVTXOCohortRequest{Requests: requests}, nil

	case clientCommandTrigger:
		return decodeDurableTrigger(m.Payload)

	default:
		return nil, fmt.Errorf("unknown durable client command: %d",
			m.Kind)
	}
}
