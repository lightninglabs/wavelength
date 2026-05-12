package main

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/lightninglabs/darepo-client/sdk/walletdk"
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7DFFB2"))
	tabStyle = lipgloss.NewStyle().
			Padding(0, 1).
			Foreground(lipgloss.Color("#9CA3AF"))
	activeTabStyle = lipgloss.NewStyle().
			Padding(0, 1).
			Bold(true).
			Foreground(lipgloss.Color("#111827")).
			Background(lipgloss.Color("#7DFFB2"))
	panelStyle = lipgloss.NewStyle().
			Border(lipgloss.ASCIIBorder()).
			Padding(1, 2)
	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#93C5FD"))
	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F87171"))
	okStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#7DFFB2"))
	warnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FBBF24"))
	mutedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9CA3AF"))
)

// keyMap contains the dashboard key bindings.
type keyMap struct {
	NextView  key.Binding
	PrevView  key.Binding
	Refresh   key.Binding
	Create    key.Binding
	Unlock    key.Binding
	Address   key.Binding
	Receive   key.Binding
	Send      key.Binding
	Copy      key.Binding
	Submit    key.Binding
	Cancel    key.Binding
	Help      key.Binding
	Quit      key.Binding
	FieldUp   key.Binding
	FieldDown key.Binding
}

// defaultKeys returns the wallet dashboard key bindings.
func defaultKeys() keyMap {
	return keyMap{
		NextView: key.NewBinding(
			key.WithKeys("tab"), key.WithHelp("tab", "next"),
		),
		PrevView: key.NewBinding(
			key.WithKeys("shift+tab"),
			key.WithHelp("shift+tab", "prev"),
		),
		Refresh: key.NewBinding(
			key.WithKeys("r"), key.WithHelp("r", "refresh"),
		),
		Create: key.NewBinding(
			key.WithKeys("c"), key.WithHelp("c", "create"),
		),
		Unlock: key.NewBinding(
			key.WithKeys("u"), key.WithHelp("u", "unlock"),
		),
		Address: key.NewBinding(
			key.WithKeys("a"), key.WithHelp("a", "address"),
		),
		Receive: key.NewBinding(
			key.WithKeys("n"), key.WithHelp("n", "receive"),
		),
		Send: key.NewBinding(
			key.WithKeys("p"), key.WithHelp("p", "send"),
		),
		Copy: key.NewBinding(
			key.WithKeys("ctrl+y"), key.WithHelp("ctrl+y", "copy"),
		),
		Submit: key.NewBinding(
			key.WithKeys("enter"), key.WithHelp("enter", "submit"),
		),
		Cancel: key.NewBinding(
			key.WithKeys("esc"), key.WithHelp("esc", "cancel"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"), key.WithHelp("?", "help"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit"),
		),
		FieldUp: key.NewBinding(
			key.WithKeys("up"), key.WithHelp("up", "field up"),
		),
		FieldDown: key.NewBinding(
			key.WithKeys("down"),
			key.WithHelp("down", "field down"),
		),
	}
}

// ShortHelp returns compact key help.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{
		k.NextView, k.PrevView, k.Refresh, k.Copy, k.Help, k.Quit,
	}
}

// FullHelp returns expanded key help groups.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{
			k.NextView,
			k.PrevView,
			k.Refresh,
		},
		{
			k.Create,
			k.Unlock,
			k.Address,
		},
		{
			k.Receive,
			k.Send,
			k.Copy,
			k.Submit,
		},
		{
			k.FieldUp,
			k.FieldDown,
			k.Cancel,
			k.Help,
			k.Quit,
		},
	}
}

// viewportShim wraps the Bubbles viewport with append helpers.
//
//nolint:recvcheck // Append mutates; Update follows Bubble Tea value style.
type viewportShim struct {
	model viewport.Model
	lines []string
}

// newViewportShim creates a scrolling viewport.
func newViewportShim(width, height int) viewportShim {
	model := viewport.New()
	model.SetWidth(width)
	model.SetHeight(height)
	model.SoftWrap = true

	return viewportShim{
		model: model,
	}
}

// Append adds one activity line and scrolls to the newest entry.
func (v *viewportShim) Append(line string) {
	v.lines = append(v.lines, line)
	if len(v.lines) > 500 {
		v.lines = v.lines[len(v.lines)-500:]
	}
	v.model.SetContent(strings.Join(v.lines, "\n"))
	v.model.GotoBottom()
}

// SetSize updates the viewport dimensions.
func (v *viewportShim) SetSize(width, height int) {
	v.model.SetWidth(width)
	v.model.SetHeight(height)
}

// Update delegates viewport key handling.
func (v viewportShim) Update(msg tea.Msg) (viewportShim, tea.Cmd) {
	var cmd tea.Cmd
	v.model, cmd = v.model.Update(msg)

	return v, cmd
}

// View renders the activity viewport.
func (v viewportShim) View() string {
	if len(v.lines) == 0 {
		return mutedStyle.Render("No activity yet.")
	}

	return v.model.View()
}

// renderHeader renders the top runtime status line.
func (m walletModel) renderHeader() string {
	network := "unknown"
	height := uint32(0)
	walletReady := false
	serverConnected := false
	if m.info != nil {
		network = m.info.Network
		height = m.info.BlockHeight
		walletReady = m.info.WalletReady
		serverConnected = m.info.ServerConnected
	}

	state := "idle"
	if m.busy > 0 {
		state = m.spinner.View() + " " + m.status
	} else if m.status != "" {
		state = m.status
	}

	left := titleStyle.Render("walletdk")
	right := fmt.Sprintf("%s height=%d wallet=%s server=%s", network,
		height, boolLabel(walletReady), boolLabel(serverConnected))

	return lipgloss.JoinHorizontal(
		lipgloss.Top, left, "  ", mutedStyle.Render(right),
		"  ", warnStyle.Render(state),
	)
}

// renderTabs renders the navigation row.
func (m walletModel) renderTabs() string {
	names := []string{
		"Overview", "Wallet", "Receive", "Send", "Swaps", "Activity",
		"Logs",
	}

	parts := make([]string, len(names))
	for i, name := range names {
		if walletView(i) == m.view {
			parts[i] = activeTabStyle.Render(name)
		} else {
			parts[i] = tabStyle.Render(name)
		}
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}

// renderBody renders the active tab contents.
func (m walletModel) renderBody() string {
	switch m.view {
	case viewWallet:
		return m.renderWallet()

	case viewReceive:
		return m.renderReceive()

	case viewSend:
		return m.renderSend()

	case viewSwaps:
		return m.renderSwaps()

	case viewActivity:
		return m.renderActivity()

	case viewLogs:
		return m.renderLogs()

	default:
		return m.renderOverview()
	}
}

// renderOverview renders status, balance, and latest details.
func (m walletModel) renderOverview() string {
	left := panelStyle.Width(panelWidth(m.width)).Render(
		m.renderInfoBlock() + "\n\n" + m.renderBalanceBlock(),
	)
	right := panelStyle.Width(panelWidth(m.width)).Render(
		m.renderDetailBlock(),
	)

	if m.width < 100 {
		return lipgloss.JoinVertical(lipgloss.Left, left, right)
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, left, "  ", right)
}

// renderWallet renders wallet create/unlock controls.
func (m walletModel) renderWallet() string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("Wallet actions"))
	b.WriteString("\n\n")
	b.WriteString(
		"Press c to create a new wallet, u to unlock an existing ",
	)
	b.WriteString("wallet, or a to create an on-chain address.\n\n")

	switch m.action {
	case actionCreate:
		b.WriteString("Create wallet\n")
		b.WriteString(m.walletPassword.View())
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render("Enter submits. Esc cancels."))

	case actionUnlock:
		b.WriteString("Unlock wallet\n")
		b.WriteString(m.walletPassword.View())
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render("Enter submits. Esc cancels."))

	default:
		b.WriteString(m.renderDetailBlock())
	}

	return panelStyle.Width(contentWidth(m.width)).Render(b.String())
}

// renderReceive renders the receive form.
func (m walletModel) renderReceive() string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("Receive over Lightning into Ark"))
	b.WriteString("\n\n")
	b.WriteString("Amount\n")
	b.WriteString(m.receiveAmount.View())
	b.WriteString("\n\n")
	b.WriteString(mutedStyle.Render("Enter starts a receive swap."))
	if m.detailTitle != "" {
		b.WriteString("\n\n")
		b.WriteString(m.renderDetailBlock())
	}

	return panelStyle.Width(contentWidth(m.width)).Render(b.String())
}

// renderSend renders the send form.
func (m walletModel) renderSend() string {
	var b strings.Builder
	b.WriteString(labelStyle.Render("Pay a Lightning invoice from Ark"))
	b.WriteString("\n\n")
	b.WriteString("Invoice\n")
	b.WriteString(m.sendInvoice.View())
	b.WriteString("\n\n")
	b.WriteString("Max fee\n")
	b.WriteString(m.sendFee.View())
	b.WriteString("\n\n")
	b.WriteString(
		mutedStyle.Render(
			"Up/down changes field. Enter starts payment.",
		),
	)
	if m.detailTitle != "" {
		b.WriteString("\n\n")
		b.WriteString(m.renderDetailBlock())
	}

	return panelStyle.Width(contentWidth(m.width)).Render(b.String())
}

// renderSwaps renders the swap accounting table.
func (m walletModel) renderSwaps() string {
	if len(m.swaps) == 0 {
		return panelStyle.Width(contentWidth(m.width)).Render(
			mutedStyle.Render("No swaps yet."),
		)
	}

	return panelStyle.Width(contentWidth(m.width)).Render(
		m.swapTable.View(),
	)
}

// renderActivity renders the scrollable activity log.
func (m walletModel) renderActivity() string {
	return panelStyle.Width(contentWidth(m.width)).Render(m.activity.View())
}

// renderLogs renders captured embedded daemon logs.
func (m walletModel) renderLogs() string {
	if len(m.logs.lines) == 0 {
		return panelStyle.Width(contentWidth(m.width)).Render(
			mutedStyle.Render("No daemon logs captured yet."),
		)
	}

	return panelStyle.Width(contentWidth(m.width)).Render(m.logs.View())
}

// renderInfoBlock renders daemon readiness fields.
func (m walletModel) renderInfoBlock() string {
	if m.info == nil {
		return mutedStyle.Render("Daemon info loading.")
	}

	return strings.Join([]string{
		labelStyle.Render("Runtime"),
		"network: " + m.info.Network,
		fmt.Sprintf("height: %d", m.info.BlockHeight),
		"wallet: " + m.info.WalletType + " ready=" +
			boolLabel(m.info.WalletReady),
		"server: " + boolLabel(m.info.ServerConnected),
		"identity: " + emptyFallback(
			shortText(m.info.IdentityPubKey, 32),
		),
	}, "\n")
}

// renderBalanceBlock renders balance buckets.
func (m walletModel) renderBalanceBlock() string {
	if m.balance == nil {
		return mutedStyle.Render("Balance loading.")
	}

	return strings.Join([]string{
		labelStyle.Render("Balance"),
		fmt.Sprintf("boarding confirmed: %s sat",
			formatSat(m.balance.BoardingConfirmedSat)),
		fmt.Sprintf("boarding unconfirmed: %s sat",
			formatSat(m.balance.BoardingUnconfirmedSat)),
		fmt.Sprintf("vtxo: %s sat",
			formatSat(m.balance.VTXOBalanceSat)),
		fmt.Sprintf("total confirmed: %s sat",
			formatSat(m.balance.TotalConfirmedSat)),
		fmt.Sprintf("on-chain wallet: %s sat",
			formatSat(m.balance.OnchainWalletConfirmedSat)),
	}, "\n")
}

// renderDetailBlock renders the latest output or a helpful empty state.
func (m walletModel) renderDetailBlock() string {
	if m.detailTitle == "" {
		return mutedStyle.Render(
			"Latest results appear here: addresses, invoices, " +
				"payment hashes, and wallet seed words.",
		)
	}

	body := wrapDetail(m.detailBody)
	if m.detailCopyText != "" {
		body += "\n\n" + mutedStyle.Render(
			"Press ctrl+y to copy "+m.detailCopyLabel+".",
		)
	}

	return labelStyle.Render(m.detailTitle) + "\n" + body
}

// renderStatus renders the footer status and error line.
func (m walletModel) renderStatus() string {
	if m.lastError != "" {
		return errorStyle.Render("error: " + m.lastError)
	}
	if m.status == "" {
		return okStyle.Render("ready")
	}

	return okStyle.Render(m.status)
}

// swapColumns returns responsive swap table columns.
func swapColumns(width int) []table.Column {
	hashWidth := 14
	stateWidth := 14
	if width > 120 {
		hashWidth = 22
		stateWidth = 20
	}

	return []table.Column{
		{
			Title: "Dir",
			Width: 8,
		},
		{
			Title: "Hash",
			Width: hashWidth,
		},
		{
			Title: "State",
			Width: stateWidth,
		},
		{
			Title: "Pending",
			Width: 8,
		},
		{
			Title: "Amount",
			Width: 12,
		},
		{
			Title: "Fee",
			Width: 10,
		},
	}
}

// mnemonicBody formats wallet seed words for one-time display.
func mnemonicBody(result *walletdk.CreateWalletResult) string {
	if result == nil {
		return ""
	}

	lines := make([]string, 0, len(result.Mnemonic)+2)
	for i, word := range result.Mnemonic {
		lines = append(lines, fmt.Sprintf("%2d. %s", i+1, word))
	}
	lines = append(lines, "", "identity: "+result.IdentityPubKey)

	return strings.Join(lines, "\n")
}

// boolLabel renders a boolean as a short status token.
func boolLabel(v bool) string {
	if v {
		return "yes"
	}

	return "no"
}

// formatSat formats signed satoshi values.
func formatSat(v int64) string {
	return strconvFormatInt(v)
}

// formatUint formats unsigned satoshi values.
func formatUint(v uint64) string {
	return fmt.Sprintf("%d", v)
}

// strconvFormatInt wraps integer formatting for consistent UI use.
func strconvFormatInt(v int64) string {
	return fmt.Sprintf("%d", v)
}

// shortHash returns a compact hash prefix.
func shortHash(hash string) string {
	return shortText(hash, 14)
}

// shortText returns a readable prefix with an ellipsis marker.
func shortText(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	if limit <= 3 {
		return s[:limit]
	}

	return s[:limit-3] + "..."
}

// compactCopyValue keeps long canonical values readable in bordered panels.
func compactCopyValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 72 {
		return emptyFallback(s)
	}

	return s[:44] + "..." + s[len(s)-20:] +
		fmt.Sprintf(" (%d chars)", len(s))
}

// emptyFallback renders a placeholder for empty strings.
func emptyFallback(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// wrapDetail wraps long details without making the app depend on clipboard
// support or QR rendering.
func wrapDetail(s string) string {
	if s == "" {
		return "-"
	}

	return lipgloss.Wrap(s, 88, " ")
}

// panelWidth returns one column width for overview panels.
func panelWidth(width int) int {
	if width <= 0 {
		return 44
	}
	if width < 100 {
		return contentWidth(width)
	}

	return (width - 8) / 2
}

// contentWidth returns the main content width.
func contentWidth(width int) int {
	if width <= 0 {
		return 88
	}
	if width < 44 {
		return 40
	}

	return width - 6
}

// clamp constrains a value to an inclusive range.
func clamp(v, minValue, maxValue int) int {
	if v < minValue {
		return minValue
	}
	if v > maxValue {
		return maxValue
	}

	return v
}
