package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/lightninglabs/darepo-client/sdk/walletdk"
)

const (
	refreshInterval = 5 * time.Second
	rpcTimeout      = 30 * time.Second
	walletTimeout   = 60 * time.Second
)

var swapReconnectDelay = 3 * time.Second

type walletView int

const (
	viewOverview walletView = iota
	viewWallet
	viewReceive
	viewSend
	viewSwaps
	viewActivity
	viewLogs
)

type walletAction int

const (
	actionNone walletAction = iota
	actionCreate
	actionUnlock
	actionAddress
	actionReceive
	actionSend
)

const (
	sendInvoiceFocus = iota
	sendFeeFocus
)

// walletClient is the narrow walletdk surface the TUI needs.
type walletClient interface {
	Stop() error

	GetInfo(context.Context) (*walletdk.Info, error)

	CreateWallet(context.Context,
		walletdk.CreateWalletRequest) (
		*walletdk.CreateWalletResult,
		error,
	)

	UnlockWallet(context.Context,
		walletdk.UnlockWalletRequest) (
		*walletdk.UnlockWalletResult,
		error,
	)

	Balance(context.Context) (*walletdk.Balance, error)

	Deposit(context.Context,
		walletdk.DepositRequest) (*walletdk.DepositResult, error)

	Receive(context.Context,
		walletdk.ReceiveRequest) (*walletdk.ReceiveResult, error)

	Send(context.Context,
		walletdk.SendRequest) (*walletdk.SendResult, error)

	List(context.Context,
		walletdk.ListRequest) (*walletdk.ListResult, error)

	Subscribe(context.Context, walletdk.SubscribeRequest) (
		<-chan walletdk.Entry, <-chan error, error)
}

// walletModel holds the Bubble Tea state for the wallet dashboard.
//
//nolint:recvcheck // Bubble Tea uses value updates; helpers mutate copies.
type walletModel struct {
	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc
	client walletClient

	keys    keyMap
	help    help.Model
	spinner spinner.Model

	width  int
	height int
	view   walletView

	info        *walletdk.Info
	balance     *walletdk.Balance
	entries     []walletdk.Entry
	swapUpdates <-chan walletdk.Entry
	swapErrs    <-chan error

	swapTable table.Model
	activity  viewportShim
	logs      viewportShim
	logLines  <-chan string

	walletPassword textinput.Model
	receiveAmount  textinput.Model
	sendInvoice    textarea.Model
	sendFee        textinput.Model
	sendFocus      int

	action          walletAction
	busy            int
	status          string
	lastError       string
	detailTitle     string
	detailBody      string
	detailCopyLabel string
	detailCopyText  string
}

// newWalletModel creates the full-screen dashboard model.
func newWalletModel(ctx context.Context, client walletClient,
	logLines <-chan string) walletModel {

	modelCtx, cancel := context.WithCancel(ctx)

	helpModel := help.New()
	helpModel.ShowAll = false

	spin := spinner.New(spinner.WithSpinner(spinner.Line))

	password := textinput.New()
	password.Placeholder = "wallet password"
	password.EchoMode = textinput.EchoPassword
	password.EchoCharacter = '*'
	password.CharLimit = 256

	receiveAmount := textinput.New()
	receiveAmount.Placeholder = "amount in sats"
	receiveAmount.CharLimit = 18

	fee := textinput.New()
	fee.Placeholder = "max fee sats (optional)"
	fee.CharLimit = 18

	invoice := textarea.New()
	invoice.Placeholder = "BOLT-11 invoice"
	invoice.ShowLineNumbers = false
	invoice.Prompt = ""
	invoice.SetHeight(4)

	swapTable := table.New(
		table.WithFocused(true), table.WithHeight(8),
		table.WithColumns(
			swapColumns(100),
		),
	)

	return walletModel{
		ctx:            modelCtx,
		cancel:         cancel,
		client:         client,
		keys:           defaultKeys(),
		help:           helpModel,
		spinner:        spin,
		view:           viewOverview,
		swapTable:      swapTable,
		activity:       newViewportShim(80, 10),
		logs:           newViewportShim(80, 10),
		logLines:       logLines,
		walletPassword: password,
		receiveAmount:  receiveAmount,
		sendInvoice:    invoice,
		sendFee:        fee,
		status:         "starting walletdk",
	}
}

// Init starts live refresh, swap subscription, and spinner ticks.
func (m walletModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick, m.refreshAllCmd(), m.refreshTickCmd(),
		m.connectSwapsCmd(), m.readLogCmd(),
	)
}

// Update applies Bubble Tea messages to the wallet model.
func (m walletModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)

	case tea.KeyPressMsg:
		next, cmd := m.handleKey(msg)
		m = next
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}

	case refreshTickMsg:
		cmds = append(cmds, m.refreshAllCmd(), m.refreshTickCmd())

	case infoMsg:
		m.handleInfo(msg)

	case balanceMsg:
		m.handleBalance(msg)

	case swapsMsg:
		m.handleSwaps(msg)

	case swapSubscriptionMsg:
		cmds = append(cmds, m.handleSwapSubscription(msg)...)

	case swapUpdateMsg:
		cmds = append(cmds, m.handleSwapUpdate(msg)...)

	case swapReconnectMsg:
		if m.ctx.Err() == nil {
			cmds = append(cmds, m.connectSwapsCmd())
		}

	case logLineMsg:
		cmds = append(cmds, m.handleLogLine(msg)...)

	case createWalletMsg:
		cmds = append(cmds, m.handleCreateWallet(msg)...)

	case unlockWalletMsg:
		cmds = append(cmds, m.handleUnlockWallet(msg)...)

	case addressMsg:
		cmds = append(cmds, m.handleAddress(msg)...)

	case receiveMsg:
		cmds = append(cmds, m.handleReceive(msg)...)

	case sendMsg:
		cmds = append(cmds, m.handleSend(msg)...)
	}

	if m.view == viewSwaps && !m.formActive() {
		var cmd tea.Cmd
		m.swapTable, cmd = m.swapTable.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if m.view == viewActivity {
		var cmd tea.Cmd
		m.activity, cmd = m.activity.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if m.view == viewLogs {
		var cmd tea.Cmd
		m.logs, cmd = m.logs.Update(msg)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return m, tea.Batch(cmds...)
}

// View renders the full-screen wallet dashboard.
func (m walletModel) View() tea.View {
	var b strings.Builder

	b.WriteString(m.renderHeader())
	b.WriteString("\n")
	b.WriteString(m.renderTabs())
	b.WriteString("\n\n")
	b.WriteString(m.renderBody())
	b.WriteString("\n")
	b.WriteString(m.renderStatus())
	b.WriteString("\n")
	b.WriteString(m.help.View(m.keys))

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.WindowTitle = "walletdk"

	return v
}

// handleKey routes key presses through global and view-specific handlers.
func (m walletModel) handleKey(msg tea.KeyPressMsg) (walletModel, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.cancel()

		return m, tea.Quit

	case key.Matches(msg, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll

		return m, nil

	case key.Matches(msg, m.keys.NextView):
		m.setView(m.view + 1)

		return m, nil

	case key.Matches(msg, m.keys.PrevView):
		m.setView(m.view - 1)

		return m, nil

	case key.Matches(msg, m.keys.Cancel):
		m.cancelForm()

		return m, nil

	case key.Matches(msg, m.keys.Copy):
		return m.copyDetail()
	}

	if m.formActive() {
		return m.handleFormKey(msg)
	}

	switch {
	case key.Matches(msg, m.keys.Refresh):
		m.status = "refreshing"

		return m, m.refreshAllCmd()

	case key.Matches(msg, m.keys.Create):
		m.setView(viewWallet)
		m.startWalletAction(actionCreate)

		return m, nil

	case key.Matches(msg, m.keys.Unlock):
		m.setView(viewWallet)
		m.startWalletAction(actionUnlock)

		return m, nil

	case key.Matches(msg, m.keys.Address):
		if m.actionInFlight() {
			return m, nil
		}
		m.action = actionAddress
		m.busy++
		m.status = "requesting address"

		return m, m.addressCmd()

	case key.Matches(msg, m.keys.Receive):
		m.setView(viewReceive)

		return m, nil

	case key.Matches(msg, m.keys.Send):
		m.setView(viewSend)

		return m, nil
	}

	return m, nil
}

// handleFormKey routes keys while an input form owns the keyboard.
func (m walletModel) handleFormKey(msg tea.KeyPressMsg) (walletModel, tea.Cmd) {
	if m.action == actionCreate || m.action == actionUnlock {
		if key.Matches(msg, m.keys.Submit) {
			return m.submitWalletForm()
		}

		var cmd tea.Cmd
		m.walletPassword, cmd = m.walletPassword.Update(msg)

		return m, cmd
	}

	switch m.view {
	case viewReceive:
		if key.Matches(msg, m.keys.Submit) {
			return m.submitReceiveForm()
		}

		var cmd tea.Cmd
		m.receiveAmount, cmd = m.receiveAmount.Update(msg)

		return m, cmd

	case viewSend:
		switch {
		case key.Matches(msg, m.keys.Submit):
			return m.submitSendForm()

		case key.Matches(msg, m.keys.FieldUp):
			m.focusSendField(sendInvoiceFocus)

			return m, nil

		case key.Matches(msg, m.keys.FieldDown):
			m.focusSendField(sendFeeFocus)

			return m, nil
		}

		var cmd tea.Cmd
		if m.sendFocus == sendInvoiceFocus {
			m.sendInvoice, cmd = m.sendInvoice.Update(msg)
		} else {
			m.sendFee, cmd = m.sendFee.Update(msg)
		}

		return m, cmd

	case viewOverview, viewWallet, viewSwaps, viewActivity, viewLogs:
		return m, nil
	}

	return m, nil
}

// submitWalletForm starts create or unlock from the password form.
func (m walletModel) submitWalletForm() (walletModel, tea.Cmd) {
	if m.actionInFlight() {
		return m, nil
	}

	password := m.walletPassword.Value()
	if password == "" {
		m.setError("wallet password is required")

		return m, nil
	}

	m.busy++
	if m.action == actionCreate {
		m.status = "creating wallet"

		return m, m.createWalletCmd(password)
	}

	m.status = "unlocking wallet"

	return m, m.unlockWalletCmd(password)
}

// submitReceiveForm starts a receive swap from the amount input.
func (m walletModel) submitReceiveForm() (walletModel, tea.Cmd) {
	if m.actionInFlight() {
		return m, nil
	}

	amount, err := strconv.ParseUint(
		strings.TrimSpace(
			m.receiveAmount.Value(),
		),
		10,
		64,
	)
	if err != nil || amount == 0 {
		m.setError("amount must be a positive integer")

		return m, nil
	}

	m.action = actionReceive
	m.busy++
	m.status = "starting receive"

	return m, m.receiveCmd(amount)
}

// submitSendForm starts a pay swap from the invoice and fee inputs.
func (m walletModel) submitSendForm() (walletModel, tea.Cmd) {
	if m.actionInFlight() {
		return m, nil
	}

	invoice := strings.TrimSpace(m.sendInvoice.Value())
	if invoice == "" {
		m.setError("invoice is required")

		return m, nil
	}

	var maxFee uint64
	feeText := strings.TrimSpace(m.sendFee.Value())
	if feeText != "" {
		parsed, err := strconv.ParseUint(feeText, 10, 64)
		if err != nil {
			m.setError("max fee must be a positive integer")

			return m, nil
		}
		maxFee = parsed
	}

	m.action = actionSend
	m.busy++
	m.status = "starting send"

	return m, m.sendCmd(invoice, maxFee)
}

// actionInFlight reports and records that another action already owns submit.
func (m *walletModel) actionInFlight() bool {
	if m.busy == 0 {
		return false
	}

	m.status = "action already running"

	return true
}

// handleInfo stores the latest readiness snapshot.
func (m *walletModel) handleInfo(msg infoMsg) {
	if msg.err != nil {
		m.setError(msg.err.Error())

		return
	}

	m.info = msg.info
	m.clearError()
}

// handleBalance stores the latest balance snapshot.
func (m *walletModel) handleBalance(msg balanceMsg) {
	if msg.err != nil {
		m.setError(msg.err.Error())

		return
	}

	m.balance = msg.balance
	m.clearError()
}

// handleSwaps stores the latest swap accounting snapshot.
func (m *walletModel) handleSwaps(msg swapsMsg) {
	if msg.err != nil {
		m.setError(msg.err.Error())

		return
	}

	m.entries = msg.entries
	m.updateSwapRows()
	m.clearError()
}

// handleSwapSubscription stores the live swap stream and starts reading it.
func (m *walletModel) handleSwapSubscription(
	msg swapSubscriptionMsg) []tea.Cmd {

	if msg.err != nil {
		if !errors.Is(msg.err, context.Canceled) {
			m.setError(msg.err.Error())
			m.addActivity(
				"activity subscription error: " +
					msg.err.Error(),
			)
		}

		return nil
	}

	m.swapUpdates = msg.updates
	m.swapErrs = msg.errs

	return []tea.Cmd{m.readSwapUpdateCmd()}
}

// handleSwapUpdate applies one live swap update and waits for the next one.
func (m *walletModel) handleSwapUpdate(msg swapUpdateMsg) []tea.Cmd {
	if msg.err != nil {
		if !errors.Is(msg.err, context.Canceled) {
			m.setError(msg.err.Error())
			m.addActivity(
				"activity subscription error: " +
					msg.err.Error(),
			)
		}

		return nil
	}
	if msg.empty {
		m.status = "activity stream closed; reconnecting"
		m.addActivity("activity stream closed; reconnecting")

		return []tea.Cmd{m.reconnectSwapsCmd()}
	}

	m.upsertSwap(msg.entry)
	m.updateSwapRows()
	m.addActivity(
		fmt.Sprintf(
			"%s %s status=%s", msg.entry.Kind,
			shortHash(msg.entry.ID), msg.entry.Status,
		),
	)

	return []tea.Cmd{m.readSwapUpdateCmd()}
}

// handleLogLine appends one daemon log line and waits for the next one.
func (m *walletModel) handleLogLine(msg logLineMsg) []tea.Cmd {
	if msg.closed {
		return nil
	}

	m.logs.Append(msg.line)

	return []tea.Cmd{m.readLogCmd()}
}

// handleCreateWallet applies create wallet results and refreshes account state.
func (m *walletModel) handleCreateWallet(msg createWalletMsg) []tea.Cmd {
	m.finishAction()
	m.walletPassword.SetValue("")

	if msg.err != nil {
		m.setError(msg.err.Error())

		return nil
	}

	m.status = "wallet created"
	m.detailTitle = "mnemonic"
	m.detailBody = mnemonicBody(msg.result)
	m.detailCopyLabel = ""
	m.detailCopyText = ""
	m.addActivity("wallet created: " + msg.result.IdentityPubKey)
	m.walletPassword.Blur()

	return []tea.Cmd{m.refreshAllCmd()}
}

// handleUnlockWallet applies unlock results and refreshes account state.
func (m *walletModel) handleUnlockWallet(msg unlockWalletMsg) []tea.Cmd {
	m.finishAction()
	m.walletPassword.SetValue("")

	if msg.err != nil {
		m.setError(msg.err.Error())

		return nil
	}

	m.status = "wallet unlocked"
	m.detailTitle = "identity"
	m.detailBody = msg.result.IdentityPubKey
	m.detailCopyLabel = "identity"
	m.detailCopyText = msg.result.IdentityPubKey
	m.addActivity("wallet unlocked: " + msg.result.IdentityPubKey)
	m.walletPassword.Blur()

	return []tea.Cmd{m.refreshAllCmd()}
}

// handleAddress applies a newly allocated boarding address.
func (m *walletModel) handleAddress(msg addressMsg) []tea.Cmd {
	m.finishAction()

	if msg.err != nil {
		m.setError(msg.err.Error())

		return nil
	}

	m.status = "address created"
	m.detailTitle = "on-chain address"
	m.detailBody = msg.result.Address
	m.detailCopyLabel = "address"
	m.detailCopyText = msg.result.Address
	m.upsertSwap(msg.result.Entry)
	m.updateSwapRows()
	m.addActivity("address created: " + msg.result.Address)

	return []tea.Cmd{m.refreshAllCmd()}
}

// handleReceive applies receive swap results and refreshes accounting.
func (m *walletModel) handleReceive(msg receiveMsg) []tea.Cmd {
	m.finishAction()

	if msg.err != nil {
		m.setError(msg.err.Error())

		return nil
	}

	m.status = "receive started"
	m.receiveAmount.SetValue("")
	m.detailTitle = "receive invoice"
	m.detailBody = fmt.Sprintf("entry_id: %s\ninvoice: %s",
		msg.result.Entry.ID, compactCopyValue(msg.result.Invoice))
	m.detailCopyLabel = "invoice"
	m.detailCopyText = msg.result.Invoice
	m.upsertSwap(msg.result.Entry)
	m.updateSwapRows()
	m.addActivity("receive started: " + msg.result.Entry.ID)

	return []tea.Cmd{m.refreshAllCmd()}
}

// handleSend applies send swap results and refreshes accounting.
func (m *walletModel) handleSend(msg sendMsg) []tea.Cmd {
	m.finishAction()

	if msg.err != nil {
		m.setError(msg.err.Error())

		return nil
	}

	m.status = "send started"
	m.sendInvoice.SetValue("")
	m.sendFee.SetValue("")
	m.detailTitle = "send"
	m.detailBody = "entry_id: " + msg.result.Entry.ID
	m.detailCopyLabel = "entry id"
	m.detailCopyText = msg.result.Entry.ID
	m.upsertSwap(msg.result.Entry)
	m.updateSwapRows()
	m.addActivity("send started: " + msg.result.Entry.ID)

	return []tea.Cmd{m.refreshAllCmd()}
}

// setView changes the active tab and applies input focus.
func (m *walletModel) setView(next walletView) {
	views := int(viewLogs) + 1
	for next < 0 {
		next += walletView(views)
	}
	next = walletView(int(next) % views)

	m.view = next
	m.walletPassword.Blur()
	m.receiveAmount.Blur()
	m.sendInvoice.Blur()
	m.sendFee.Blur()

	switch next {
	case viewReceive:
		m.action = actionReceive
		_ = m.receiveAmount.Focus()

	case viewSend:
		m.action = actionSend
		m.focusSendField(m.sendFocus)

	default:
		if m.action == actionReceive || m.action == actionSend {
			m.action = actionNone
		}
	}
}

// startWalletAction focuses the wallet password form for create or unlock.
func (m *walletModel) startWalletAction(action walletAction) {
	m.action = action
	m.detailTitle = ""
	m.detailBody = ""
	m.detailCopyLabel = ""
	m.detailCopyText = ""
	_ = m.walletPassword.Focus()
}

// cancelForm clears the active form or detail panel.
func (m *walletModel) cancelForm() {
	m.action = actionNone
	m.detailTitle = ""
	m.detailBody = ""
	m.detailCopyLabel = ""
	m.detailCopyText = ""
	m.walletPassword.SetValue("")
	m.walletPassword.Blur()
	m.receiveAmount.SetValue("")
	m.receiveAmount.Blur()
	m.sendInvoice.SetValue("")
	m.sendInvoice.Blur()
	m.sendFee.SetValue("")
	m.sendFee.Blur()
	m.sendFocus = sendInvoiceFocus
}

// copyDetail copies the current result's canonical value to the terminal
// clipboard instead of asking users to select wrapped text from a panel.
func (m walletModel) copyDetail() (walletModel, tea.Cmd) {
	if m.detailCopyText == "" {
		m.status = "nothing to copy"

		return m, nil
	}

	m.status = "copied " + m.detailCopyLabel
	m.addActivity("copied " + m.detailCopyLabel)

	return m, tea.SetClipboard(m.detailCopyText)
}

// formActive returns true when a view should receive text input.
func (m walletModel) formActive() bool {
	if m.action == actionCreate || m.action == actionUnlock {
		return true
	}

	switch m.view {
	case viewReceive:
		return m.receiveAmount.Focused()

	case viewSend:
		return m.sendInvoice.Focused() || m.sendFee.Focused()

	default:
		return false
	}
}

// focusSendField switches focus between invoice and max-fee fields.
func (m *walletModel) focusSendField(field int) {
	m.sendFocus = field
	m.sendInvoice.Blur()
	m.sendFee.Blur()
	if field == sendFeeFocus {
		_ = m.sendFee.Focus()

		return
	}

	_ = m.sendInvoice.Focus()
}

// finishAction decrements the in-flight action counter.
func (m *walletModel) finishAction() {
	if m.busy > 0 {
		m.busy--
	}
	m.action = actionNone
}

// setError records a visible error and logs it to activity.
func (m *walletModel) setError(err string) {
	if err == "" {
		return
	}

	m.lastError = err
	m.status = "error"
	m.addActivity("error: " + err)
}

// clearError clears any visible error.
func (m *walletModel) clearError() {
	m.lastError = ""
}

// addActivity appends one line to the activity viewport.
func (m *walletModel) addActivity(line string) {
	if line == "" {
		return
	}

	m.activity.Append(time.Now().Format("15:04:05") + " " + line)
}

// upsertSwap updates or appends a wallet activity entry by id.
func (m *walletModel) upsertSwap(next walletdk.Entry) {
	for i, entry := range m.entries {
		if entry.ID == next.ID {
			m.entries[i] = next

			return
		}
	}

	m.entries = append([]walletdk.Entry{next}, m.entries...)
}

// updateSwapRows renders wallet activity entries into the table component.
func (m *walletModel) updateSwapRows() {
	rows := make([]table.Row, 0, len(m.entries))
	for _, entry := range m.entries {
		rows = append(rows, table.Row{
			string(entry.Kind),
			shortHash(entry.ID),
			string(entry.Status),
			formatSat(entry.AmountSat),
			formatSat(entry.FeeSat),
			shortText(entry.Counterparty, 18),
		})
	}

	m.swapTable.SetRows(rows)
}

// resize updates component dimensions from the terminal size.
func (m *walletModel) resize(width, height int) {
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 32
	}

	m.width = width
	m.height = height
	m.help.SetWidth(width)
	m.receiveAmount.SetWidth(clamp(width-8, 20, 72))
	m.walletPassword.SetWidth(clamp(width-8, 20, 72))
	m.sendInvoice.SetWidth(clamp(width-8, 30, 100))
	m.sendFee.SetWidth(clamp(width-8, 20, 72))

	tableHeight := clamp(height-18, 6, 16)
	m.swapTable.SetHeight(tableHeight)
	m.swapTable.SetWidth(width - 4)
	m.swapTable.SetColumns(swapColumns(width))

	activityHeight := clamp(height-18, 6, 18)
	m.activity.SetSize(width-4, activityHeight)
	m.logs.SetSize(width-4, activityHeight)
}

// refreshAllCmd refreshes the core dashboard snapshots.
func (m walletModel) refreshAllCmd() tea.Cmd {
	return tea.Batch(
		m.infoCmd(),
		m.balanceCmd(),
		m.swapsCmd(),
	)
}

// refreshTickCmd schedules the next periodic refresh.
func (m walletModel) refreshTickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return refreshTickMsg(t)
	})
}

// infoCmd fetches daemon readiness.
func (m walletModel) infoCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, rpcTimeout)
		defer cancel()

		info, err := m.client.GetInfo(ctx)

		return infoMsg{info: info, err: err}
	}
}

// balanceCmd fetches wallet balances.
func (m walletModel) balanceCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, rpcTimeout)
		defer cancel()

		balance, err := m.client.Balance(ctx)

		return balanceMsg{balance: balance, err: err}
	}
}

// swapsCmd fetches wallet activity entries.
func (m walletModel) swapsCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, rpcTimeout)
		defer cancel()

		result, err := m.client.List(ctx, walletdk.ListRequest{})
		if result == nil {
			result = &walletdk.ListResult{}
		}

		return swapsMsg{entries: result.Entries, err: err}
	}
}

// connectSwapsCmd opens the daemon wallet activity subscription once.
func (m walletModel) connectSwapsCmd() tea.Cmd {
	return func() tea.Msg {
		updates, errs, err := m.client.Subscribe(
			m.ctx, walletdk.SubscribeRequest{
				IncludeExisting: true,
			},
		)
		if err != nil {
			return swapSubscriptionMsg{err: err}
		}

		return swapSubscriptionMsg{
			updates: updates,
			errs:    errs,
		}
	}
}

// reconnectSwapsCmd spaces out reconnects when the stream closes cleanly.
func (m walletModel) reconnectSwapsCmd() tea.Cmd {
	return tea.Tick(swapReconnectDelay, func(time.Time) tea.Msg {
		return swapReconnectMsg{}
	})
}

// readSwapUpdateCmd waits for one update from the active activity stream.
func (m walletModel) readSwapUpdateCmd() tea.Cmd {
	return func() tea.Msg {
		select {
		case entry, ok := <-m.swapUpdates:
			if !ok {
				return swapUpdateMsg{empty: true}
			}

			return swapUpdateMsg{entry: entry}

		case err, ok := <-m.swapErrs:
			if !ok || err == nil {
				return swapUpdateMsg{empty: true}
			}

			return swapUpdateMsg{err: err}

		case <-m.ctx.Done():
			return swapUpdateMsg{err: context.Canceled}
		}
	}
}

// readLogCmd waits for one captured daemon log line.
func (m walletModel) readLogCmd() tea.Cmd {
	if m.logLines == nil {
		return nil
	}

	return func() tea.Msg {
		select {
		case line, ok := <-m.logLines:
			return logLineMsg{
				line:   line,
				closed: !ok,
			}

		case <-m.ctx.Done():
			return logLineMsg{closed: true}
		}
	}
}

// createWalletCmd creates or imports the daemon wallet.
func (m walletModel) createWalletCmd(password string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, walletTimeout)
		defer cancel()

		result, err := m.client.CreateWallet(
			ctx, walletdk.CreateWalletRequest{
				WalletPassword: []byte(password),
			},
		)

		return createWalletMsg{result: result, err: err}
	}
}

// unlockWalletCmd unlocks an existing daemon wallet.
func (m walletModel) unlockWalletCmd(password string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, walletTimeout)
		defer cancel()

		result, err := m.client.UnlockWallet(
			ctx, walletdk.UnlockWalletRequest{
				WalletPassword: []byte(password),
			},
		)

		return unlockWalletMsg{result: result, err: err}
	}
}

// addressCmd allocates a fresh boarding address.
func (m walletModel) addressCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, rpcTimeout)
		defer cancel()

		result, err := m.client.Deposit(ctx, walletdk.DepositRequest{})

		return addressMsg{result: result, err: err}
	}
}

// receiveCmd creates a Lightning invoice payable into the wallet.
func (m walletModel) receiveCmd(amount uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, rpcTimeout)
		defer cancel()

		result, err := m.client.Receive(ctx, walletdk.ReceiveRequest{
			AmountSat: amount,
		})

		return receiveMsg{result: result, err: err}
	}
}

// sendCmd starts an outbound wallet payment.
func (m walletModel) sendCmd(invoice string, maxFee uint64) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, rpcTimeout)
		defer cancel()

		result, err := m.client.Send(ctx, walletdk.SendRequest{
			Invoice:   invoice,
			MaxFeeSat: maxFee,
		})

		return sendMsg{result: result, err: err}
	}
}

type refreshTickMsg time.Time

type infoMsg struct {
	info *walletdk.Info
	err  error
}

type balanceMsg struct {
	balance *walletdk.Balance
	err     error
}

type swapsMsg struct {
	entries []walletdk.Entry
	err     error
}

type swapSubscriptionMsg struct {
	updates <-chan walletdk.Entry
	errs    <-chan error
	err     error
}

type swapUpdateMsg struct {
	entry walletdk.Entry
	err   error
	empty bool
}

type swapReconnectMsg struct{}

type logLineMsg struct {
	line   string
	closed bool
}

type createWalletMsg struct {
	result *walletdk.CreateWalletResult
	err    error
}

type unlockWalletMsg struct {
	result *walletdk.UnlockWalletResult
	err    error
}

type addressMsg struct {
	result *walletdk.DepositResult
	err    error
}

type receiveMsg struct {
	result *walletdk.ReceiveResult
	err    error
}

type sendMsg struct {
	result *walletdk.SendResult
	err    error
}
