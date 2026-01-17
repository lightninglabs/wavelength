package gen

// Config configures mailbox RPC stub generation.
type Config struct {
	// ExcludeService skips generating stubs for a proto service.
	//
	// For example: "mailbox.v1.MailboxService".
	ExcludeService string
}
