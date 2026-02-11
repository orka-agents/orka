/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tui

import "github.com/charmbracelet/lipgloss"

var (
	// Pane borders
	coordinatorBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("39")).
				Padding(0, 1)

	agentBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("243")).
				Padding(0, 1)

	// Agent status indicators
	statusPending   = lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Render("⏸")
	statusRunning   = lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render("▶")
	statusSucceeded = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓")
	statusFailed    = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("✗")

	// Tool call styling
	toolCallStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	toolNameStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))

	// Title styles
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))

	// Status bar
	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243")).
			Padding(0, 1)

	// Selected agent in list
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))

	// Notification style (for transitions)
	notificationStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243")).Italic(true)

	// Peek overlay
	peekBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(lipgloss.Color("39")).
			Padding(1, 2)
)
