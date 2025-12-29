package main

import (
	"github.com/charmbracelet/lipgloss"
)

// Color palette - minimal and modern
var (
	primaryColor   = lipgloss.Color("39")  // Bright blue
	successColor   = lipgloss.Color("42")  // Green
	warningColor   = lipgloss.Color("214") // Orange
	errorColor     = lipgloss.Color("196") // Red
	mutedColor     = lipgloss.Color("241") // Gray
	highlightColor = lipgloss.Color("51")  // Cyan
)

// Text styles
var (
	// Muted text for secondary info
	mutedStyle = lipgloss.NewStyle().Foreground(mutedColor)

	// Primary accent for important elements
	primaryStyle = lipgloss.NewStyle().Foreground(primaryColor)

	// Success style for completed actions
	successStyle = lipgloss.NewStyle().Foreground(successColor)

	// Warning style
	warningStyle = lipgloss.NewStyle().Foreground(warningColor)

	// Error style
	errorStyle = lipgloss.NewStyle().Foreground(errorColor)

	// Bold style for emphasis
	boldStyle = lipgloss.NewStyle().Bold(true)

	// Command/code style
	codeStyle = lipgloss.NewStyle().
			Foreground(highlightColor)

	// Tool execution prefix
	toolPrefixStyle = lipgloss.NewStyle().
			Foreground(mutedColor)

	// Tool output style
	toolOutputStyle = lipgloss.NewStyle().
			Foreground(mutedColor)
)

// List/menu styles
var (
	// Selected item in a list
	selectedStyle = lipgloss.NewStyle().
			Foreground(primaryColor).
			Bold(true)

	// Unselected item
	unselectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	// Cursor/indicator
	cursorStyle = lipgloss.NewStyle().
			Foreground(primaryColor)

	// Checkmark for enabled items
	checkStyle = lipgloss.NewStyle().
			Foreground(successColor)

	// Disabled/unchecked items
	uncheckStyle = lipgloss.NewStyle().
			Foreground(mutedColor)
)

// Box/container styles
var (
	// Question box
	questionStyle = lipgloss.NewStyle().
			Foreground(primaryColor).
			Bold(true)

	// Help text at bottom of prompts
	helpStyle = lipgloss.NewStyle().
			Foreground(mutedColor).
			Italic(true)

	// Thinking/spinner text
	spinnerStyle = lipgloss.NewStyle().
			Foreground(mutedColor)
)

// Helper functions for common patterns
func muted(s string) string {
	return mutedStyle.Render(s)
}

func success(s string) string {
	return successStyle.Render(s)
}

func warn(s string) string {
	return warningStyle.Render(s)
}

func errorf(s string) string {
	return errorStyle.Render(s)
}

func code(s string) string {
	return codeStyle.Render(s)
}

func bold(s string) string {
	return boldStyle.Render(s)
}
