// Package tui provides a terminal UI for browsing the blame timeline.
// It uses Bubble Tea (github.com/charmbracelet/bubbletea) and renders in any
// terminal that supports ANSI colours.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/prem0x01/costblame/pkg/models"
)

// Store is the subset of store.Store required by the TUI.
type Store interface {
	RecentBlameEdges(ctx context.Context, limit int) ([]models.BlameEdge, error)
}

// --- styles ---

var (
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	highStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red  — high confidence
	midStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // orange
	lowStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244")) // grey
	titleStyle  = lipgloss.NewStyle().Bold(true).Padding(0, 1).
			Background(lipgloss.Color("62")).Foreground(lipgloss.Color("230"))
)

// --- messages ---

type edgesLoadedMsg struct{ edges []models.BlameEdge }
type errMsg struct{ err error }

// --- model ---

// Model is the Bubble Tea model for the blame timeline view.
type Model struct {
	store   Store
	table   table.Model
	spinner spinner.Model
	edges   []models.BlameEdge
	loading bool
	err     error
	detail  *models.BlameEdge // nil = list view, non-nil = detail view
}

// New creates the root TUI model.
func New(store Store) Model {
	cols := []table.Column{
		{Title: "Service", Width: 22},
		{Title: "Delta", Width: 10},
		{Title: "Score", Width: 8},
		{Title: "PR", Width: 8},
		{Title: "Author", Width: 16},
		{Title: "When", Width: 18},
		{Title: "Status", Width: 12},
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithFocused(true),
		table.WithHeight(20),
	)
	t.SetStyles(table.Styles{
		Header:   headerStyle,
		Selected: lipgloss.NewStyle().Bold(true).Background(lipgloss.Color("62")),
	})

	s := spinner.New()
	s.Spinner = spinner.Dot

	return Model{
		store:   store,
		table:   t,
		spinner: s,
		loading: true,
	}
}

// Init kicks off the initial data load.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.loadEdges())
}

func (m Model) loadEdges() tea.Cmd {
	return func() tea.Msg {
		edges, err := m.store.RecentBlameEdges(context.Background(), 50)
		if err != nil {
			return errMsg{err}
		}
		return edgesLoadedMsg{edges}
	}
}

// Update handles incoming messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "enter":
			if m.detail == nil && len(m.edges) > 0 {
				idx := m.table.Cursor()
				if idx < len(m.edges) {
					edge := m.edges[idx]
					m.detail = &edge
				}
			} else {
				m.detail = nil
			}
		case "r":
			m.loading = true
			return m, tea.Batch(m.spinner.Tick, m.loadEdges())
		case "esc":
			m.detail = nil
		}

	case edgesLoadedMsg:
		m.edges = msg.edges
		m.loading = false
		m.table.SetRows(edgesToRows(msg.edges))
		return m, nil

	case errMsg:
		m.err = msg.err
		m.loading = false
		return m, nil

	case spinner.TickMsg:
		if m.loading {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}

	if m.detail == nil {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd
	}
	return m, nil
}

// View renders the current UI state.
func (m Model) View() string {
	if m.loading {
		return titleStyle.Render(" costblame ") + "\n\n" +
			m.spinner.View() + " Loading blame timeline...\n"
	}
	if m.err != nil {
		return titleStyle.Render(" costblame ") + "\n\nError: " + m.err.Error() + "\n\nPress q to quit."
	}
	if m.detail != nil {
		return m.renderDetail(m.detail)
	}
	return m.renderList()
}

func (m Model) renderList() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(" costblame — blame timeline ") + "\n\n")
	b.WriteString(m.table.View())
	b.WriteString("\n\n")
	b.WriteString(lowStyle.Render("↑/↓ navigate • enter detail • r refresh • q quit"))
	return b.String()
}

func (m Model) renderDetail(edge *models.BlameEdge) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(" costblame — blame detail ") + "\n\n")

	if edge.CostSnapshot != nil {
		snap := edge.CostSnapshot
		b.WriteString(fmt.Sprintf("Service    : %s\n", snap.Service))
		b.WriteString(fmt.Sprintf("Period     : %s\n", snap.PeriodStart.Format("Jan 2 2006 15:04 UTC")))
		b.WriteString(fmt.Sprintf("Amount     : $%.2f (was $%.2f, %+.1f%%)\n",
			snap.AmountUSD, snap.PrevAmountUSD, snap.DeltaPct))
		b.WriteString(fmt.Sprintf("Anomaly    : %.2f σ\n\n", snap.AnomalyScore))
	}

	if edge.DeployEvent != nil {
		deploy := edge.DeployEvent
		b.WriteString(fmt.Sprintf("PR #%d      : %s\n", deploy.PRNumber, deploy.PRTitle))
		b.WriteString(fmt.Sprintf("Author     : @%s (%s)\n", deploy.PRAuthor, deploy.PRTeam))
		b.WriteString(fmt.Sprintf("Deployed   : %s\n", deploy.OccurredAt.Format("Jan 2 2006 15:04 UTC")))
		b.WriteString(fmt.Sprintf("Repository : %s\n\n", deploy.Repository))
	}

	b.WriteString(fmt.Sprintf("Confidence : %.0f%%\n", edge.ConfidenceScore*100))
	b.WriteString("Factors:\n")
	for _, f := range edge.ConfidenceFactors {
		b.WriteString(fmt.Sprintf("  %-22s %.2f  %s\n", f.Name, f.Score, f.Reason))
	}

	if edge.Narrative != "" {
		b.WriteString("\nNarrative:\n")
		b.WriteString(wrapText(edge.Narrative, 72))
		b.WriteString("\n")
	}

	b.WriteString("\n" + lowStyle.Render("esc / enter → back • q quit"))
	return b.String()
}

func edgesToRows(edges []models.BlameEdge) []table.Row {
	rows := make([]table.Row, 0, len(edges))
	for _, e := range edges {
		service, delta, pr, author, when := "(unknown)", "-", "-", "-", "-"
		if e.CostSnapshot != nil {
			service = e.CostSnapshot.Service
			delta = fmt.Sprintf("%+.1f%%", e.CostSnapshot.DeltaPct)
		}
		if e.DeployEvent != nil {
			pr = fmt.Sprintf("#%d", e.DeployEvent.PRNumber)
			author = "@" + e.DeployEvent.PRAuthor
			when = timeAgo(e.DeployEvent.OccurredAt)
		}
		score := scoreStyle(e.ConfidenceScore, fmt.Sprintf("%.0f%%", e.ConfidenceScore*100))
		rows = append(rows, table.Row{service, delta, score, pr, author, when, string(e.Status)})
	}
	return rows
}

func scoreStyle(score float64, text string) string {
	switch {
	case score >= 0.65:
		return highStyle.Render(text)
	case score >= 0.35:
		return midStyle.Render(text)
	default:
		return lowStyle.Render(text)
	}
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%.0fm ago", d.Minutes())
	case d < 24*time.Hour:
		return fmt.Sprintf("%.0fh ago", d.Hours())
	default:
		return t.Format("Jan 2")
	}
}

func wrapText(text string, width int) string {
	words := strings.Fields(text)
	var lines []string
	var current strings.Builder
	for _, w := range words {
		if current.Len()+len(w)+1 > width && current.Len() > 0 {
			lines = append(lines, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteByte(' ')
		}
		current.WriteString(w)
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	return strings.Join(lines, "\n")
}
