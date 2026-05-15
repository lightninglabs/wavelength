package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/lightninglabs/darepo-client/sdk/walletdk"
	"github.com/stretchr/testify/require"
)

// requireWalletModel unwraps Bubble Tea models returned by Update.
func requireWalletModel(t *testing.T, model tea.Model) walletModel {
	t.Helper()

	next, ok := model.(walletModel)
	require.True(t, ok)

	return next
}

// TestWalletModelRefreshMessagesPopulateState verifies refresh message
// handling.
func TestWalletModelRefreshMessagesPopulateState(t *testing.T) {
	model := newWalletModel(t.Context(), newFakeWalletClient(), nil)

	next, _ := model.Update(infoMsg{info: &walletdk.Info{
		Network:         "regtest",
		BlockHeight:     144,
		WalletType:      "lwwallet",
		WalletReady:     true,
		ServerConnected: true,
		IdentityPubKey:  "abcdef",
	}})
	model = requireWalletModel(t, next)
	require.Equal(t, "regtest", model.info.Network)

	next, _ = model.Update(balanceMsg{balance: &walletdk.Balance{
		ConfirmedSat: 42,
	}})
	model = requireWalletModel(t, next)
	require.EqualValues(t, 42, model.balance.ConfirmedSat)

	entry := walletdk.Entry{
		ID:        "0123456789abcdef",
		Kind:      walletdk.EntryKindSend,
		Status:    walletdk.EntryStatusComplete,
		AmountSat: -10_000,
	}
	next, _ = model.Update(swapsMsg{entries: []walletdk.Entry{entry}})
	model = requireWalletModel(t, next)

	require.Len(t, model.entries, 1)
	require.Len(t, model.swapTable.Rows(), 1)
	require.Equal(t, "send", model.swapTable.Rows()[0][0])
}

// TestWalletModelReceiveValidationDoesNotCallClient verifies local validation.
func TestWalletModelReceiveValidationDoesNotCallClient(t *testing.T) {
	fake := newFakeWalletClient()
	model := newWalletModel(t.Context(), fake, nil)
	model.setView(viewReceive)
	model.receiveAmount.SetValue("not-a-number")

	next, cmd := model.Update(keyPress("enter"))
	model = requireWalletModel(t, next)

	require.Nil(t, cmd)
	require.Equal(t, 0, fake.receiveCalls)
	require.Contains(t, model.lastError, "amount")
}

// TestWalletModelReceiveSuccessUpdatesAccounting verifies receive actions.
func TestWalletModelReceiveSuccessUpdatesAccounting(t *testing.T) {
	fake := newFakeWalletClient()
	invoice := "lnbcrt" + strings.Repeat("a", 120) + "z"
	fake.receiveResult = &walletdk.ReceiveResult{
		Invoice: invoice,
		Entry: walletdk.Entry{
			ID:        "hash",
			Kind:      walletdk.EntryKindReceive,
			Status:    walletdk.EntryStatusPending,
			AmountSat: 123,
		},
	}

	model := newWalletModel(t.Context(), fake, nil)
	model.setView(viewReceive)
	model.receiveAmount.SetValue("123")

	next, cmd := model.Update(keyPress("enter"))
	model = requireWalletModel(t, next)
	require.NotNil(t, cmd)
	require.Equal(t, 1, model.busy)

	msg := cmd()
	next, _ = model.Update(msg)
	model = requireWalletModel(t, next)

	require.Equal(t, 1, fake.receiveCalls)
	require.Zero(t, model.busy)
	require.Empty(t, model.receiveAmount.Value())
	require.Contains(t, model.detailBody, "invoice")
	require.NotContains(t, model.detailBody, invoice)
	require.Equal(t, "invoice", model.detailCopyLabel)
	require.Equal(t, invoice, model.detailCopyText)
	require.Len(t, model.entries, 1)

	next, cmd = model.Update(keyPress("ctrl+y"))
	model = requireWalletModel(t, next)

	require.NotNil(t, cmd)
	require.Equal(t, "copied invoice", model.status)
}

// TestWalletModelCopyWithoutDetailIsNoop verifies copy has a helpful status.
func TestWalletModelCopyWithoutDetailIsNoop(t *testing.T) {
	model := newWalletModel(t.Context(), newFakeWalletClient(), nil)

	next, cmd := model.Update(keyPress("ctrl+y"))
	model = requireWalletModel(t, next)

	require.Nil(t, cmd)
	require.Equal(t, "nothing to copy", model.status)
}

// TestWalletModelCreateClearsPassword verifies sensitive form cleanup.
func TestWalletModelCreateClearsPassword(t *testing.T) {
	fake := newFakeWalletClient()
	fake.createResult = &walletdk.CreateWalletResult{
		Mnemonic: []string{
			"alpha",
			"bravo",
		},
		IdentityPubKey: "identity",
	}

	model := newWalletModel(t.Context(), fake, nil)
	next, _ := model.Update(keyPress("c"))
	model = requireWalletModel(t, next)
	model.walletPassword.SetValue("secret")

	next, cmd := model.Update(keyPress("enter"))
	model = requireWalletModel(t, next)
	require.NotNil(t, cmd)

	msg := cmd()
	next, _ = model.Update(msg)
	model = requireWalletModel(t, next)

	require.Equal(t, 1, fake.createCalls)
	require.Empty(t, model.walletPassword.Value())
	require.Contains(t, model.detailBody, "alpha")
	require.Contains(t, model.detailBody, "identity")
}

// TestWalletModelSwapSubscriptionUpdatesTable verifies live accounting updates.
func TestWalletModelSwapSubscriptionUpdatesTable(t *testing.T) {
	model := newWalletModel(t.Context(), newFakeWalletClient(), nil)
	updates := make(chan walletdk.Entry, 1)
	errs := make(chan error, 1)

	next, cmd := model.Update(swapSubscriptionMsg{
		updates: updates,
		errs:    errs,
	})
	model = requireWalletModel(t, next)
	require.NotNil(t, cmd)

	updates <- walletdk.Entry{
		ID:        "abcdef",
		Kind:      walletdk.EntryKindSend,
		Status:    walletdk.EntryStatusComplete,
		AmountSat: -100,
	}

	msg := cmd()
	next, _ = model.Update(msg)
	model = requireWalletModel(t, next)

	require.Len(t, model.entries, 1)
	require.Equal(t, "complete", model.swapTable.Rows()[0][2])
}

// TestWalletModelReconnectsSwapSubscriptionAfterDelay verifies that a closed
// stream schedules a delayed reconnect instead of opening a new stream inline.
func TestWalletModelReconnectsSwapSubscriptionAfterDelay(t *testing.T) {
	oldDelay := swapReconnectDelay
	swapReconnectDelay = time.Nanosecond
	t.Cleanup(func() {
		swapReconnectDelay = oldDelay
	})

	fake := newFakeWalletClient()
	model := newWalletModel(t.Context(), fake, nil)

	cmds := model.handleSwapUpdate(swapUpdateMsg{empty: true})
	require.Len(t, cmds, 1)
	require.Equal(t, 0, fake.subscribeCalls)
	require.Equal(t, "activity stream closed; reconnecting", model.status)

	msg := cmds[0]()
	require.IsType(t, swapReconnectMsg{}, msg)
	require.Equal(t, 0, fake.subscribeCalls)
}

// TestWalletModelCancelClearsReceiveForm verifies Esc leaves receive input
// mode with no stale value.
func TestWalletModelCancelClearsReceiveForm(t *testing.T) {
	model := newWalletModel(t.Context(), newFakeWalletClient(), nil)
	model.setView(viewReceive)
	model.receiveAmount.SetValue("50000")

	require.True(t, model.formActive())

	next, _ := model.Update(keyPress("esc"))
	model = requireWalletModel(t, next)

	require.Equal(t, actionNone, model.action)
	require.Empty(t, model.receiveAmount.Value())
	require.False(t, model.receiveAmount.Focused())
	require.False(t, model.formActive())
}

// TestWalletModelCancelClearsSendForm verifies Esc leaves send input mode with
// no stale invoice, fee, or alternate focus.
func TestWalletModelCancelClearsSendForm(t *testing.T) {
	model := newWalletModel(t.Context(), newFakeWalletClient(), nil)
	model.setView(viewSend)
	model.sendInvoice.SetValue("lnbcrt1invoice")
	model.sendFee.SetValue("123")
	model.focusSendField(sendFeeFocus)

	require.True(t, model.formActive())

	next, _ := model.Update(keyPress("esc"))
	model = requireWalletModel(t, next)

	require.Equal(t, actionNone, model.action)
	require.Empty(t, model.sendInvoice.Value())
	require.Empty(t, model.sendFee.Value())
	require.False(t, model.sendInvoice.Focused())
	require.False(t, model.sendFee.Focused())
	require.Equal(t, sendInvoiceFocus, model.sendFocus)
	require.False(t, model.formActive())
}

// TestWalletModelLogLineUpdatesLogsTab verifies captured logs are displayed.
func TestWalletModelLogLineUpdatesLogsTab(t *testing.T) {
	logs := make(chan string, 1)
	model := newWalletModel(t.Context(), newFakeWalletClient(), logs)

	cmd := model.readLogCmd()
	require.NotNil(t, cmd)

	logs <- "daemon log line"
	next, cmd := model.Update(cmd())
	model = requireWalletModel(t, next)

	require.NotNil(t, cmd)
	require.Contains(t, model.logs.View(), "daemon log line")
}

// TestWalletModelQuitCancelsContext verifies quit stops background work.
func TestWalletModelQuitCancelsContext(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()

	model := newWalletModel(parent, newFakeWalletClient(), nil)
	next, cmd := model.Update(keyPress("q"))
	model = requireWalletModel(t, next)

	require.NotNil(t, cmd)
	require.ErrorIs(t, model.ctx.Err(), context.Canceled)
}

// TestLogSinkCapturesLines verifies daemon writes are split into TUI log lines.
func TestLogSinkCapturesLines(t *testing.T) {
	sink := newLogSink(2)
	n, err := sink.Write([]byte("one\ntwo\npartial"))
	require.NoError(t, err)
	require.Equal(t, len("one\ntwo\npartial"), n)

	require.Equal(t, "one", <-sink.Lines())
	require.Equal(t, "two", <-sink.Lines())

	_, err = sink.Write([]byte(" three\n"))
	require.NoError(t, err)
	require.Equal(t, "partial three", <-sink.Lines())
}

// keyPress builds a Bubble Tea v2 key press for tests.
func keyPress(key string) tea.KeyPressMsg {
	switch key {
	case "enter":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})

	case "tab":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyTab})

	case "esc":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape})

	case "ctrl+y":
		return tea.KeyPressMsg(tea.Key{
			Code: 'y',
			Mod:  tea.ModCtrl,
		})

	default:
		runes := []rune(key)
		if len(runes) == 0 {
			return tea.KeyPressMsg(tea.Key{})
		}

		return tea.KeyPressMsg(tea.Key{
			Text: key,
			Code: runes[0],
		})
	}
}

// fakeWalletClient is an in-memory walletClient for model tests.
type fakeWalletClient struct {
	createCalls    int
	unlockCalls    int
	addressCalls   int
	receiveCalls   int
	sendCalls      int
	subscribeCalls int

	createResult  *walletdk.CreateWalletResult
	unlockResult  *walletdk.UnlockWalletResult
	addressResult *walletdk.DepositResult
	receiveResult *walletdk.ReceiveResult
	sendResult    *walletdk.SendResult

	info    *walletdk.Info
	balance *walletdk.Balance
	entries []walletdk.Entry
	err     error
}

// newFakeWalletClient returns a fake wallet client with useful defaults.
func newFakeWalletClient() *fakeWalletClient {
	return &fakeWalletClient{
		createResult: &walletdk.CreateWalletResult{
			Mnemonic: []string{
				"alpha",
				"bravo",
			},
			IdentityPubKey: "identity",
		},
		unlockResult: &walletdk.UnlockWalletResult{
			IdentityPubKey: "identity",
		},
		addressResult: &walletdk.DepositResult{
			Address: "bcrt1address",
		},
		receiveResult: &walletdk.ReceiveResult{
			Invoice: "invoice",
			Entry: walletdk.Entry{
				ID:   "receive",
				Kind: walletdk.EntryKindReceive,
			},
		},
		sendResult: &walletdk.SendResult{
			Entry: walletdk.Entry{
				ID:   "send",
				Kind: walletdk.EntryKindSend,
			},
		},
		info: &walletdk.Info{
			Network:         "regtest",
			WalletReady:     true,
			ServerConnected: true,
		},
		balance: &walletdk.Balance{},
	}
}

// Stop satisfies walletClient.
func (f *fakeWalletClient) Stop() error {
	return nil
}

// GetInfo satisfies walletClient.
func (f *fakeWalletClient) GetInfo(context.Context) (*walletdk.Info, error) {
	return f.info, f.err
}

// CreateWallet satisfies walletClient.
func (f *fakeWalletClient) CreateWallet(context.Context,
	walletdk.CreateWalletRequest) (*walletdk.CreateWalletResult, error) {

	f.createCalls++

	return f.createResult, f.err
}

// UnlockWallet satisfies walletClient.
func (f *fakeWalletClient) UnlockWallet(context.Context,
	walletdk.UnlockWalletRequest) (*walletdk.UnlockWalletResult, error) {

	f.unlockCalls++

	return f.unlockResult, f.err
}

// Balance satisfies walletClient.
func (f *fakeWalletClient) Balance(context.Context) (*walletdk.Balance, error) {
	return f.balance, f.err
}

// Deposit satisfies walletClient.
func (f *fakeWalletClient) Deposit(context.Context, walletdk.DepositRequest) (
	*walletdk.DepositResult, error) {

	f.addressCalls++

	return f.addressResult, f.err
}

// Receive satisfies walletClient.
func (f *fakeWalletClient) Receive(context.Context, walletdk.ReceiveRequest) (
	*walletdk.ReceiveResult, error) {

	f.receiveCalls++

	return f.receiveResult, f.err
}

// Send satisfies walletClient.
func (f *fakeWalletClient) Send(context.Context, walletdk.SendRequest) (
	*walletdk.SendResult, error) {

	f.sendCalls++

	return f.sendResult, f.err
}

// List satisfies walletClient.
func (f *fakeWalletClient) List(context.Context, walletdk.ListRequest) (
	*walletdk.ListResult, error) {

	return &walletdk.ListResult{Entries: f.entries}, f.err
}

// Subscribe satisfies walletClient.
func (f *fakeWalletClient) Subscribe(context.Context,
	walletdk.SubscribeRequest) (<-chan walletdk.Entry, <-chan error,
	error) {

	f.subscribeCalls++

	if f.err != nil {
		return nil, nil, f.err
	}

	updates := make(chan walletdk.Entry)
	errs := make(chan error, 1)
	errs <- errors.New("not connected")

	return updates, errs, nil
}
