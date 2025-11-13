# Development Guidelines
1. [Code Documentation and Commenting](#code-documentation-and-commenting)
1. [Code Spacing and Formatting](#code-spacing-and-formatting)
1. [Additional Style Constraints](#additional-style-constraints)
1. [Recommended settings for your editor](#recommended-settings-for-your-editor)

## Why this emphasis on formatting?

Code in general (and Open Source code specifically) is _read_ by developers many
more times during its lifecycle than it is modified. With this fact in mind, the
Golang language was designed for readability (among other goals).
While the enforced formatting of `go fmt` and some best practices already
eliminate many discussions, the resulting code can still look and feel very
differently among different developers.

We aim to enforce a few additional rules to unify the look and feel of all code
in `lnd` to help improve the overall readability.

## Code Documentation and Commenting

- At a minimum every function must be commented with its intended purpose and
  any assumptions that it makes
- Function comments must always begin with the name of the function per
    [Effective Go](https://golang.org/doc/effective_go.html)
- Function comments should be complete sentences since they allow a wide
    variety of automated presentations such as [godoc.org](https://godoc.org)
- The general rule of thumb is to look at it as if you were completely
  unfamiliar with the code and ask yourself, would this give me enough
  information to understand what this function does and how I'd probably want to
  use it?
- Exported functions should also include detailed information the caller of the
  function will likely need to know and/or understand:<br /><br />

**WRONG**
```go
// generates a revocation key
func DeriveRevocationPubkey(commitPubKey *btcec.PublicKey,
	revokePreimage []byte) *btcec.PublicKey {
```
**RIGHT**
```go
// DeriveRevocationPubkey derives the revocation public key given the
// counterparty's commitment key, and revocation preimage derived via a
// pseudo-random-function. In the event that we (for some reason) broadcast a
// revoked commitment transaction, then if the other party knows the revocation
// preimage, then they'll be able to derive the corresponding private key to
// this private key by exploiting the homomorphism in the elliptic curve group:
//    * https://en.wikipedia.org/wiki/Group_homomorphism#Homomorphisms_of_abelian_groups
//
// The derivation is performed as follows:
//
//   revokeKey := commitKey + revokePoint
//             := G*k + G*h
//             := G * (k+h)
//
// Therefore, once we divulge the revocation preimage, the remote peer is able to
// compute the proper private key for the revokeKey by computing:
//   revokePriv := commitPriv + revokePreimge mod N
//
// Where N is the order of the sub-group.
func DeriveRevocationPubkey(commitPubKey *btcec.PublicKey,
	revokePreimage []byte) *btcec.PublicKey {
```
- Comments in the body of the code are highly encouraged, but they should
  explain the intention of the code as opposed to just calling out the
  obvious<br /><br />

**WRONG**
```go
// return err if amt is less than 546
if amt < 546 {
	return err
}
```
**RIGHT**
```go
// Treat transactions with amounts less than the amount which is considered dust
// as non-standard.
if amt < 546 {
	return err
}
```
**NOTE:** The above should really use a constant as opposed to a magic number,
but it was left as a magic number to show how much of a difference a good
comment can make.

## Code Spacing and formatting

Code in general (and Open Source code specifically) is _read_ by developers many
more times during its lifecycle than it is modified. With this fact in mind, the
Golang language was designed for readability (among other goals).
While the enforced formatting of `go fmt` and some best practices already
eliminate many discussions, the resulting code can still look and feel very
differently among different developers.

We aim to enforce a few additional rules to unify the look and feel of all code
in `lnd` to help improve the overall readability.

Blocks of code within `lnd` should be segmented into logical stanzas of
operation. Such spacing makes the code easier to follow at a skim, and reduces
unnecessary line noise. Coupled with the commenting scheme specified in the
[contribution guide](#code-documentation-and-commenting),
proper spacing allows readers to quickly scan code, extracting semantics quickly.
Functions should _not_ just be laid out as a bare contiguous block of code.

**WRONG**
```go
	witness := make([][]byte, 4)
	witness[0] = nil
	if bytes.Compare(pubA, pubB) == -1 {
		witness[1] = sigB
		witness[2] = sigA
	} else {
		witness[1] = sigA
		witness[2] = sigB
	}
	witness[3] = witnessScript
	return witness
```
**RIGHT**
```go
	witness := make([][]byte, 4)

	// When spending a p2wsh multi-sig script, rather than an OP_0, we add
	// a nil stack element to eat the extra pop.
	witness[0] = nil

	// When initially generating the witnessScript, we sorted the serialized
	// public keys in descending order. So we do a quick comparison in order
	// to ensure the signatures appear on the Script Virtual Machine stack in
	// the correct order.
	if bytes.Compare(pubA, pubB) == -1 {
		witness[1] = sigB
		witness[2] = sigA
	} else {
		witness[1] = sigA
		witness[2] = sigB
	}

	// Finally, add the preimage as the last witness element.
	witness[3] = witnessScript

	return witness
```

Additionally, we favor spacing between stanzas within syntax like: switch case
statements and select statements.

**WRONG**
```go
	switch {
		case a:
			<code block>
		case b:
			<code block>
		case c:
			<code block>
		case d:
			<code block>
		default:
			<code block>
	}
```
**RIGHT**
```go
	switch {
		// Brief comment detailing instances of this case (repeat below).
		case a:
			<code block>

		case b:
			<code block>

		case c:
			<code block>

		case d:
			<code block>

		default:
			<code block>
	}
```

## Additional Style Constraints

Before a PR is submitted, the proposer should ensure that the file passes the
set of linting scripts run by `make lint`. These include `gofmt`. In addition
to `gofmt` we've opted to enforce the following style guidelines.

### 80 character line length

ALL columns (on a best effort basis) should be wrapped to 80 line columns.
Editors should be set to treat a **tab as 8 spaces**.

**WRONG**
```go
myKey := "0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366a2e151fcee7"
```

**RIGHT**
```go
myKey := "0214cd678a565041d00e6cf8d62ef8add33b4af4786fb2beb87b366a2e1" +
	"51fcee7"
```

### Wrapping long function calls

When wrapping a line that contains a function call as the unwrapped line exceeds
the column limit, the close parenthesis should be placed on its own line.
Additionally, all arguments should begin in a new line after the open parenthesis.

**WRONG**
```go
value, err := bar(a,
	a, b, c)
```

**RIGHT**
```go
value, err := bar(
	a, a, b, c,
)
```

As long as the visual symmetry of the opening and closing parentheses (or curly
braces) is preserved, arguments that would otherwise introduce a new level of
indentation are allowed to be written in a more compact form.
Visual symmetry here means that when two or more opening parentheses or curly
braces are on the same line, then they must also be closed on the same line.
And the closing line needs to have the same indentation level as the opening
line.

Example with inline struct creation:

**ACCEPTABLE**
```go
	response, err := node.AddInvoice(
		ctx, &lnrpc.Invoice{
			Memo:      "invoice",
			ValueMsat: int64(oneUnitMilliSat - 1),
		},
	)
```

**PREFERRED**
```go
	response, err := node.AddInvoice(ctx, &lnrpc.Invoice{
		Memo:      "invoice",
		ValueMsat: int64(oneUnitMilliSat - 1),
	})
```

**WRONG**
```go
	response, err := node.AddInvoice(ctx, &lnrpc.Invoice{
		Memo:      "invoice",
		ValueMsat: int64(oneUnitMilliSat - 1)})
```

Example with nested function call:

**ACCEPTABLE**:
```go
	payInvoiceWithSatoshi(
		t.t, dave, invoiceResp2, withFailure(
			lnrpc.Payment_FAILED, failureNoRoute,
        ),
    )
```

**PREFERRED**:
```go
	payInvoiceWithSatoshi(t.t, dave, invoiceResp2, withFailure(
		lnrpc.Payment_FAILED, failureNoRoute,
	))
```

#### Exception for log and error message formatting

**Note that the above guidelines don't apply to log or error messages.** For
log and error messages, committers should attempt to minimize the number of
lines utilized, while still adhering to the 80-character column limit. For
example:

**WRONG**
```go
return fmt.Errorf(
	"this is a long error message with a couple (%d) place holders",
	len(things),
)

log.Debugf(
	"Something happened here that we need to log: %v",
	longVariableNameHere,
)
```

**RIGHT**
```go
return fmt.Errorf("this is a long error message with a couple (%d) place "+
	"holders", len(things))

log.Debugf("Something happened here that we need to log: %v",
	longVariableNameHere)
```

This helps to visually distinguish those formatting statements (where nothing
of consequence happens except for formatting an error message or writing
to a log) from actual method or function calls. This compact formatting should
be used for calls to formatting functions like `fmt.Errorf`,
`log.(Trace|Debug|Info|Warn|Error)f` and `fmt.Printf`.
But not for statements that are important for the flow or logic of the code,
like `require.NoErrorf()`.

#### Exceptions and additional styling for structured logging

When making use of structured logging calls (there are any `btclog.Logger` 
methods ending in `S`), a few different rules and exceptions apply.

1) **Static messages:** Structured log calls take a `context.Context` as a first
parameter and a _static_ string as the second parameter (the `msg` parameter). 
Formatted strings should ideally not be used for the construction of the `msg` 
parameter. Instead, key-value pairs (or `slog` attributes) should be used to 
provide additional variables to the log line.

**WRONG**
```go
log.DebugS(ctx, fmt.Sprintf("User %d just spent %.8f to open a channel", userID, 0.0154))
```

**RIGHT**
```go
log.InfoS(ctx, "Channel open performed",
        slog.Int("user_id", userID),
        btclog.Fmt("amount", "%.8f", 0.00154))
```

2) **Key-value attributes**: The third parameter in any structured log method is 
a variadic list of the `any` type but it is required that these are provided in 
key-value pairs such that an associated `slog.Attr` variable can be created for 
each key-value pair. The simplest way to specify this is to directly pass in the 
key-value pairs as raw literals as follows:

```go
log.InfoS(ctx, "Channel open performed", "user_id", userID, "amount", 0.00154)
```
This does work, but it becomes easy to make a mistake and accidentally leave out
a value for each key provided leading to a nonsensical log line. To avoid this, 
it is suggested to make use of the various `slog.Attr` helper functions as 
follows:

```go
log.InfoS(ctx, "Channel open performed",
        slog.Int("user_id", userID),
        btclog.Fmt("amount", "%.8f", 0.00154))
```

3) **Line wrapping**: Structured log lines are an exception to the 80-character
line wrapping rule. This is so that the key-value pairs can be easily read and
reasoned about. If it is the case that there is only a single key-value pair
and the entire log line is still less than 80 characters, it is acceptable to
have the key-value pair on the same line as the log message. However, if there
are multiple key-value pairs, it is suggested to use the one line per key-value
pair format. Due to this suggestion, it is acceptable for any single key-value
pair line to exceed 80 characters for the sake of readability.

**WRONG**
```go
// Example 1.
log.InfoS(ctx, "User connected", 
        "user_id", userID)

// Example 2.
log.InfoS(ctx, "Channel open performed", "user_id", userID,
        btclog.Fmt("amount", "%.8f", 0.00154), "channel_id", channelID)

// Example 3.
log.InfoS(ctx, "Bytes received", 
        "user_id", userID, 
        btclog.Hex("peer_id", peerID.SerializeCompressed()),
        btclog.Hex("message", []bytes{
                0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
        })))
```

**RIGHT**
```go
// Example 1.
log.InfoS(ctx, "User connected", "user_id", userID)

// Example 2.
log.InfoS(ctx, "Channel open performed",
        slog.Int("user_id", userID),
        btclog.Fmt("amount", "%.8f", 0.00154),
        slog.String("channel_id", channelID))

// Example 3.
log.InfoS(ctx, "Bytes received", 
        "user_id", userID,
        btclog.Hex("peer_id", peerID.SerializeCompressed()),
        btclog.Hex("message", []bytes{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08})))
```

### Wrapping long function definitions

If one is forced to wrap lines of function arguments that exceed the 
80-character limit, then indentation must be kept on the following lines. Also,
lines should not end with an open parenthesis if the function definition isn't
finished yet.

**WRONG**
```go
func foo(a, b, c,
) (d, error) {

func bar(a, b, c) (
	d, error,
) {

func baz(a, b, c) (
	d, error) {
```
**RIGHT**
```go
func foo(a, b,
	c) (d, error) {

func baz(a, b, c) (d,
	error) {

func longFunctionName(
	a, b, c) (d, error) {
```

If a function declaration spans multiple lines the body should start with an
empty line to help visually distinguishing the two elements.

**WRONG**
```go
func foo(a, b, c,
	d, e) error {
	var a int
}
```
**RIGHT**
```go
func foo(a, b, c,
	d, e) error {

	var a int
}
```

### Inline slice definitions

In Go a list of slices can be initialized with values directly, using curly
braces. Whenever possible, the more verbose/indented style should be used for
better readability and easier git diff handling. Because that results in more
levels of code indentation, the more compact version is allowed in situations
where the remaining space would otherwise be too restricted, resulting in too
long lines (or excessive use of the `// nolint: ll` directive).

**ACCEPTABLE**
```go
	testCases := []testCase{{
		name: "spend exactly all",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value:    1 * btcutil.SatoshiPerBitcoin,
			},
		}},
	}, {
		name: "spend more",
		coins: []wallet.Coin{{
			TxOut: wire.TxOut{
				PkScript: p2wkhScript,
				Value:    1 * btcutil.SatoshiPerBitcoin,
			},
		}},
	}}
```

**PREFERRED**
```go
	coin := btcutil.SatoshiPerBitcoin
	testCases := []testCase{
		{
			name: "spend exactly all",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1 * coin,
					},
				},
			},
		},
		{
			name: "spend more",
			coins: []wallet.Coin{
				{
					TxOut: wire.TxOut{
						PkScript: p2wkhScript,
						Value:    1 * coin,
					},
				},
			},
		},
	}
```

## Recommended settings for your editor

To make it easier to follow the rules outlined above, we recommend setting up
your editor with at least the following two settings:

1. Set your tabulator width (also called "tab size") to **8 spaces**.
2. Set a ruler or visual guide at 80 character.

Note that the two above settings are automatically applied in editors that
support the `EditorConfig` scheme (for example GoLand, GitHub, GitLab,
VisualStudio). In addition, specific settings for Visual Studio Code are checked
into the code base as well.

Other editors (for example Atom, Notepad++, Vim, Emacs and so on) might install
a plugin to understand the rules in the `.editorconfig` file.

In Vim, you might want to use `set colorcolumn=80`.
