package serverconn

import (
	mailboxconn "github.com/lightninglabs/wavelength/mailbox/conn"
	mailboxpb "github.com/lightninglabs/wavelength/mailbox/pb"
)

// validateInboundEnvelope checks that an inbound server envelope carries the
// mailbox transport and Ark protocol versions bound to this runtime. It
// returns a typed permanent version StatusError on mismatch so the ingress
// loop refuses to acknowledge or dispatch the envelope and the connector can
// classify the failure as permanent. A nil return means the envelope is
// compatible and may be delivered.
//
// There is no legacy fallback: an inbound Ark version of zero is treated as a
// mismatch like any other wrong version. The client and operator are deployed
// together, so every authentic server envelope carries the negotiated version.
func (a *ServerConnectionActor) validateInboundEnvelope(
	env *mailboxpb.Envelope) error {

	if env == nil {
		return nil
	}

	// The mailbox transport version must match the bound transport version
	// exactly. The transport is a stable code constant, so any difference
	// means the envelope cannot be safely framed for this runtime.
	if env.ProtocolVersion != a.cfg.MailboxProtocolVersion {
		return mailboxconn.NewStatusError(
			"inbound", a.inboundMismatchStatus(
				mailboxconn.StatusMailboxVersionUnsupported,
			),
		)
	}

	// The Ark protocol version must match the bound version exactly.
	if env.ArkProtocolVersion != a.cfg.ArkProtocolVersion {
		return mailboxconn.NewStatusError(
			"inbound", a.inboundMismatchStatus(
				mailboxconn.StatusArkVersionMismatch,
			),
		)
	}

	return nil
}

// inboundMismatchStatus synthesizes the structured status carried by an
// inbound version mismatch. It advertises the runtime's bound versions so the
// failure is diagnosable and classifiable as permanent by the shared status
// helper.
func (a *ServerConnectionActor) inboundMismatchStatus(
	code string) *mailboxpb.Status {

	return &mailboxpb.Status{
		Ok:      false,
		Code:    code,
		Message: "inbound envelope version mismatch",
		SupportedMailboxVersions: []uint32{
			a.cfg.MailboxProtocolVersion,
		},
		SupportedArkVersions: []uint32{
			a.cfg.ArkProtocolVersion,
		},
	}
}
