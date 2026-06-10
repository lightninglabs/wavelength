package mailboxpb

// MailboxProtocolVersionV1 is the stable mailbox transport version. The
// mailbox transport version defines how an envelope is framed, routed, and
// delivered: Envelope framing, RpcMeta routing, Send/Pull/AckUpTo behavior,
// and cursor, acknowledgement, and durable replay semantics.
//
// It is intentionally a code constant rather than operator configuration
// because mailbox v1 is the stable bootstrap endpoint that every client must
// be able to decode. A future breaking mailbox transport must run on a new
// endpoint or protobuf service package (such as mailbox.v2); it cannot depend
// on a v1 envelope to negotiate a format that v1 cannot decode. This value is
// therefore not a normal rolling configuration choice.
const MailboxProtocolVersionV1 uint32 = 1
