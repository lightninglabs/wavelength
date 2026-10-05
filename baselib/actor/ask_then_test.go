package actor

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/wavelength/build"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/stretchr/testify/require"
)

// thenMsg is both the request and the wrapped reply of the AskThen tests. A
// reply carries the result of the Ask in res.
type thenMsg struct {
	BaseMessage

	kind string
	res  fn.Result[string]
}

// MessageType returns the message type name.
func (m *thenMsg) MessageType() string {
	return "thenMsg"
}

// wrapThen maps an Ask result into a reply message for the caller.
func wrapThen(res fn.Result[string]) *thenMsg {
	return &thenMsg{kind: "reply", res: res}
}

// startThenActor builds and starts an actor around fn, and stops it when the
// test ends.
func startThenActor(t *testing.T, id string,
	f func(context.Context, *thenMsg) fn.Result[string]) *Actor[
	*thenMsg, string] {

	t.Helper()

	a := NewActor(ActorConfig[*thenMsg, string]{
		ID:          id,
		Behavior:    NewFunctionBehavior(f),
		MailboxSize: 4,
	})
	a.Start()
	t.Cleanup(a.Stop)

	return a
}

// thenCaller is a caller actor that asks a callee on "start" and records the
// wrapped reply it receives.
type thenCaller struct {
	callee  ActorRef[*thenMsg, string]
	self    TellOnlyRef[*thenMsg]
	timeout time.Duration

	// turns counts Receive calls, so a test can tell the reply was
	// handled in a later turn than the one that asked.
	turns atomic.Int64

	startTurn atomic.Int64
	replyTurn atomic.Int64

	replies chan fn.Result[string]
}

// Receive asks on "start" and forwards the wrapped reply to the test.
func (c *thenCaller) Receive(ctx context.Context,
	msg *thenMsg) fn.Result[string] {

	turn := c.turns.Add(1)

	switch msg.kind {
	case "start":
		c.startTurn.Store(turn)
		AskThen(
			ctx, c.callee, &thenMsg{
				kind: "ask",
			},
			c.self,
			c.timeout,
			wrapThen,
		)

	case "reply":
		c.replyTurn.Store(turn)
		c.replies <- msg.res
	}

	return fn.Ok("ok")
}

// runThenCaller wires a caller to callee, kicks it off, and returns the result
// of the wrapped reply.
func runThenCaller(t *testing.T, callee ActorRef[*thenMsg, string],
	timeout time.Duration) (*thenCaller, fn.Result[string]) {

	t.Helper()

	c := &thenCaller{
		callee:  callee,
		timeout: timeout,
		replies: make(chan fn.Result[string], 1),
	}
	a := NewActor(ActorConfig[*thenMsg, string]{
		ID: "then-caller", Behavior: c, MailboxSize: 4,
	})
	c.self = a.TellRef()
	a.Start()
	t.Cleanup(a.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, a.Ref().Tell(ctx, &thenMsg{kind: "start"}))

	select {
	case res := <-c.replies:
		return c, res

	case <-ctx.Done():
		t.Fatal("no reply delivered")
	}

	return nil, fn.Result[string]{}
}

// TestAskThenDeliversReply checks the success path: the reply reaches the
// caller as a message handled in a later turn than the one that asked.
func TestAskThenDeliversReply(t *testing.T) {
	t.Parallel()

	callee := startThenActor(t, "then-ok",
		func(_ context.Context, _ *thenMsg) fn.Result[string] {
			return fn.Ok("pong")
		},
	)

	c, res := runThenCaller(t, callee.Ref(), 5*time.Second)

	v, err := res.Unpack()
	require.NoError(t, err)
	require.Equal(t, "pong", v)
	require.Greater(t, c.replyTurn.Load(), c.startTurn.Load())
}

// TestAskThenDeliversCalleeError checks that an error the callee returns is
// wrapped and delivered like a value.
func TestAskThenDeliversCalleeError(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	callee := startThenActor(t, "then-err",
		func(_ context.Context, _ *thenMsg) fn.Result[string] {
			return fn.Err[string](errBoom)
		},
	)

	_, res := runThenCaller(t, callee.Ref(), 5*time.Second)

	_, err := res.Unpack()
	require.ErrorIs(t, err, errBoom)
}

// TestAskThenTimeout checks that a callee that does not answer in time yields
// a wrapped context.DeadlineExceeded, and that the delivery to self still
// succeeds although the Ask context has expired by then.
func TestAskThenTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	callee := startThenActor(t, "then-slow",
		func(_ context.Context, _ *thenMsg) fn.Result[string] {
			<-release

			return fn.Ok("late")
		},
	)

	start := time.Now()
	_, res := runThenCaller(t, callee.Ref(), 50*time.Millisecond)

	_, err := res.Unpack()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 5*time.Second)
}

// TestAskThenNonPositiveTimeout checks that a timeout that is not positive is
// reported as an expired deadline without sending anything.
func TestAskThenNonPositiveTimeout(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	callee := startThenActor(t, "then-unsent",
		func(_ context.Context, _ *thenMsg) fn.Result[string] {
			calls.Add(1)

			return fn.Ok("never")
		},
	)

	_, res := runThenCaller(t, callee.Ref(), 0)

	_, err := res.Unpack()
	require.ErrorIs(t, err, context.DeadlineExceeded)

	time.Sleep(20 * time.Millisecond)
	require.Zero(t, calls.Load())
}

// TestAskThenDropsResultForStoppedSelf checks that a result for an actor that
// has stopped is dropped instead of parking the helper goroutine.
func TestAskThenDropsResultForStoppedSelf(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	callee := startThenActor(t, "then-late",
		func(_ context.Context, _ *thenMsg) fn.Result[string] {
			<-release

			return fn.Ok("late")
		},
	)

	var wrapped atomic.Int64
	self := startThenActor(t, "then-gone",
		func(_ context.Context, _ *thenMsg) fn.Result[string] {
			return fn.Ok("ok")
		},
	)
	selfRef := self.TellRef()

	// The wrapper reports when the inner Tell returns, which is what a
	// delivery parked forever on a stopped actor would never do.
	told := make(chan struct{})
	selfWrapped := &signalTellRef{TellOnlyRef: selfRef, told: told}

	ctx := context.Background()
	AskThen(
		ctx, callee.Ref(), &thenMsg{kind: "ask"}, selfWrapped,
		5*time.Second, func(r fn.Result[string]) *thenMsg {
			wrapped.Add(1)

			return wrapThen(r)
		},
	)

	self.Stop()
	close(release)

	// The wrap runs even though the delivery cannot; it must return.
	require.Eventually(t, func() bool {
		return wrapped.Load() == 1
	}, 5*time.Second, 5*time.Millisecond)

	select {
	case <-told:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery to a stopped actor never returned")
	}
}

// signalTellRef wraps a TellOnlyRef and closes told once the inner Tell
// returns.
type signalTellRef struct {
	TellOnlyRef[*thenMsg]

	told chan struct{}
}

// Tell forwards to the wrapped ref and then signals that it returned.
func (r *signalTellRef) Tell(ctx context.Context, msg *thenMsg) error {
	err := r.TellOnlyRef.Tell(ctx, msg)
	close(r.told)

	return err
}

// TestAskThenKeepsCallerTxFromChannelCallee checks that a channel-mailbox
// callee never sees the caller's transaction when asked through AskThen,
// because the turn that owns it has ended by the time the callee runs, while
// a plain Ask from the same context still carries it.
func TestAskThenKeepsCallerTxFromChannelCallee(t *testing.T) {
	t.Parallel()

	sawTx := make(chan bool, 2)
	sawMarker := make(chan bool, 2)
	callee := startThenActor(t, "then-tx",
		func(ctx context.Context, _ *thenMsg) fn.Result[string] {
			sawTx <- HasTx(ctx)
			sawMarker <- isDetachedAsk(ctx)

			return fn.Ok("ok")
		},
	)
	self := NewChannelTellOnlyRef[*thenMsg]("then-tx-self", 1)

	// A sentinel is enough, since nothing here uses the transaction.
	ctx := WithTx(context.Background(), &sql.Tx{})
	require.True(t, HasTx(ctx))

	AskThen(
		ctx, callee.Ref(), &thenMsg{
			kind: "ask",
		},
		self,
		5*time.Second,
		wrapThen,
	)
	_, ok := self.AwaitMessage(5 * time.Second)
	require.True(t, ok)
	require.False(t, <-sawTx)

	// The marker must not leak into the callee's turn either, or the
	// callee's own plain Asks would lose a transaction it opened itself.
	require.False(t, <-sawMarker)

	// A plain Ask keeps its transaction, since its caller is parked and
	// still holds it open.
	_, err := callee.Ref().Ask(ctx, &thenMsg{kind: "ask"}).Await(
		context.Background(),
	).Unpack()
	require.NoError(t, err)
	require.True(t, <-sawTx)
	require.False(t, <-sawMarker)
}

// TestAskThenBreaksAskCycle is the shape that deadlocks with Await. Actor A
// asks B; while serving that message B needs a reply from A before it can
// finish its work. With Await on both sides A waits on B and B waits on A.
// With AskThen on both sides neither parks, so A serves B's query and B reports
// the outcome back as a message.
func TestAskThenBreaksAskCycle(t *testing.T) {
	t.Parallel()

	var (
		aRef  ActorRef[*thenMsg, string]
		bRef  ActorRef[*thenMsg, string]
		aSelf TellOnlyRef[*thenMsg]
		bSelf TellOnlyRef[*thenMsg]
	)
	result := make(chan string, 1)

	b := startThenActor(t, "cycle-b",
		func(ctx context.Context, m *thenMsg) fn.Result[string] {
			switch m.kind {
			case "need":
				AskThen(
					ctx, aRef, &thenMsg{
						kind: "query",
					},
					bSelf,
					5*time.Second,
					wrapThen,
				)

			case "reply":
				// A's answer arrived in a later turn; report
				// it.
				err := aSelf.Tell(ctx, &thenMsg{
					kind: "final",
					res: fn.Ok(
						"b-done:" + valueOf(m.res),
					),
				})
				if err != nil {
					return fn.Err[string](err)
				}
			}

			return fn.Ok("ack")
		},
	)

	a := startThenActor(t, "cycle-a",
		func(ctx context.Context, m *thenMsg) fn.Result[string] {
			switch m.kind {
			case "start":
				AskThen(
					ctx, bRef, &thenMsg{
						kind: "need",
					},
					aSelf,
					5*time.Second,
					wrapThen,
				)

			case "query":
				return fn.Ok("a-answer")

			case "final":
				result <- valueOf(m.res)
			}

			return fn.Ok("ok")
		},
	)

	// The refs are read only after the actors run, so publish them before
	// the first message goes in.
	aRef, bRef = a.Ref(), b.Ref()
	aSelf, bSelf = a.TellRef(), b.TellRef()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, a.Ref().Tell(ctx, &thenMsg{kind: "start"}))

	select {
	case v := <-result:
		require.Equal(t, "b-done:a-answer", v)

	case <-ctx.Done():
		t.Fatal("actors deadlocked")
	}
}

// valueOf returns the value of res, or the empty string for an error.
func valueOf(res fn.Result[string]) string {
	return res.UnwrapOr("")
}

// syncBuffer is a log sink that is safe to write from a helper goroutine while
// the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

// String returns what has been written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// failingTellRef is a TellOnlyRef whose Tell always fails with err.
type failingTellRef struct {
	err error
}

// ID returns a fixed identifier.
func (r *failingTellRef) ID() string {
	return "failing-self"
}

// Tell fails with the configured error.
func (r *failingTellRef) Tell(context.Context, *thenMsg) error {
	return r.err
}

// TryTell fails with the configured error.
func (r *failingTellRef) TryTell(context.Context, *thenMsg) error {
	return r.err
}

// TestAskThenLogsLostDelivery checks that a failed delivery to a live self is
// logged at warning level, while a shutdown-style failure stays at debug.
func TestAskThenLogsLostDelivery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
		not  string
	}{{
		name: "enqueue failure warns",
		err:  errors.New("enqueue: disk full"),
		want: "[WRN]",
		not:  "[DBG]",
	}, {
		name: "mailbox closed stays at debug",
		err:  ErrMailboxClosed,
		want: "[DBG]",
		not:  "[WRN]",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			callee := startThenActor(t, "then-lost",
				func(_ context.Context,
					_ *thenMsg) fn.Result[string] {

					return fn.Ok("pong")
				},
			)

			var sink syncBuffer
			log := btclog.NewSLogger(
				btclog.NewDefaultHandler(&sink),
			)
			log.SetLevel(btclog.LevelDebug)
			ctx := build.ContextWithLogger(
				context.Background(), log,
			)

			AskThen(
				ctx, callee.Ref(), &thenMsg{
					kind: "ask",
				},
				TellOnlyRef[*thenMsg](
					&failingTellRef{
						err: tc.err,
					},
				),
				5*time.Second,
				wrapThen,
			)

			require.Eventually(t, func() bool {
				return strings.Contains(sink.String(), tc.want)
			}, 5*time.Second, time.Millisecond)
			require.NotContains(t, sink.String(), tc.not)
		})
	}
}
