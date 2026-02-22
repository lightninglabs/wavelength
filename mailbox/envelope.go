package mailbox

import mailboxpb "github.com/lightninglabs/darepo-client/mailbox/pb"

// Envelope is the durable unit sent via the mailbox edge.
//
// Envelope is defined in the public mailbox contract and is used as the
// storage unit for mailbox persistence.
type Envelope = mailboxpb.Envelope
