package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ============================================================================
// Spinner Component
// ============================================================================

type spinnerModel struct {
	spinner  spinner.Model
	quitting bool
	message  string
}

func newSpinnerModel(message string) spinnerModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(primaryColor)
	return spinnerModel{
		spinner: s,
		message: message,
	}
}

func (m spinnerModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m spinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		m.quitting = true
		return m, tea.Quit
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case stopSpinnerMsg:
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m spinnerModel) View() string {
	if m.quitting {
		return ""
	}
	return fmt.Sprintf("%s %s", m.spinner.View(), spinnerStyle.Render(m.message))
}

type stopSpinnerMsg struct{}

// ============================================================================
// Selector Component (for ask_user)
// ============================================================================

type selectorModel struct {
	question string
	options  []string
	cursor   int
	selected int
	quitting bool
	canceled bool
}

func newSelectorModel(question string, options []string) selectorModel {
	return selectorModel{
		question: question,
		options:  options,
		cursor:   0,
		selected: -1,
	}
}

func (m selectorModel) Init() tea.Cmd {
	return nil
}

func (m selectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.canceled = true
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.options)-1 {
				m.cursor++
			}
		case "enter", " ":
			m.selected = m.cursor
			m.quitting = true
			return m, tea.Quit
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			idx := int(msg.String()[0] - '1')
			if idx >= 0 && idx < len(m.options) {
				m.selected = idx
				m.quitting = true
				return m, tea.Quit
			}
		}
	}
	return m, nil
}

func (m selectorModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Question
	b.WriteString("\n")
	b.WriteString(questionStyle.Render(m.question))
	b.WriteString("\n\n")

	// Options
	for i, opt := range m.options {
		cursor := "  "
		style := unselectedStyle

		if i == m.cursor {
			cursor = cursorStyle.Render("> ")
			style = selectedStyle
		}

		number := mutedStyle.Render(fmt.Sprintf("%d. ", i+1))
		b.WriteString(fmt.Sprintf("%s%s%s\n", cursor, number, style.Render(opt)))
	}

	// Help
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓ navigate • enter select • q cancel"))
	b.WriteString("\n")

	return b.String()
}

// RunSelector runs an interactive selector and returns the selected index (-1 if canceled)
func RunSelector(question string, options []string) int {
	m := newSelectorModel(question, options)
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return -1
	}
	fm := finalModel.(selectorModel)
	if fm.canceled {
		return -1
	}
	return fm.selected
}

// ============================================================================
// Toggle List Component (for -tools)
// ============================================================================

type toggleItem struct {
	name    string
	desc    string
	enabled bool
}

type toggleModel struct {
	title    string
	items    []toggleItem
	cursor   int
	quitting bool
}

func newToggleModel(title string, items []toggleItem) toggleModel {
	return toggleModel{
		title: title,
		items: items,
	}
}

func (m toggleModel) Init() tea.Cmd {
	return nil
}

func (m toggleModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc", "enter":
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case " ", "x", "t":
			// Toggle current item
			m.items[m.cursor].enabled = !m.items[m.cursor].enabled
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			idx := int(msg.String()[0] - '1')
			if idx >= 0 && idx < len(m.items) {
				m.items[idx].enabled = !m.items[idx].enabled
			}
		}
	}
	return m, nil
}

func (m toggleModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Title
	b.WriteString("\n")
	b.WriteString(boldStyle.Render(m.title))
	b.WriteString("\n\n")

	// Items
	for i, item := range m.items {
		cursor := "  "
		if i == m.cursor {
			cursor = cursorStyle.Render("> ")
		}

		check := uncheckStyle.Render("○")
		nameStyle := unselectedStyle
		if item.enabled {
			check = checkStyle.Render("●")
			nameStyle = selectedStyle
		}

		number := mutedStyle.Render(fmt.Sprintf("%d. ", i+1))
		name := nameStyle.Render(item.name)
		desc := mutedStyle.Render(" - " + item.desc)

		b.WriteString(fmt.Sprintf("%s%s %s%s%s\n", cursor, check, number, name, desc))
	}

	// Help
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓ navigate • space toggle • enter done"))
	b.WriteString("\n")

	return b.String()
}

// RunToggleList runs an interactive toggle list and returns the items with their final states
func RunToggleList(title string, items []toggleItem) []toggleItem {
	m := newToggleModel(title, items)
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return items
	}
	return finalModel.(toggleModel).items
}

// ============================================================================
// Model Picker Component (for -model)
// ============================================================================

type modelPickerModel struct {
	title       string
	models      []string
	current     string
	cursor      int
	selected    int
	quitting    bool
	showInput   bool
	customInput string
}

func newModelPickerModel(title string, models []string, current string) modelPickerModel {
	return modelPickerModel{
		title:    title,
		models:   models,
		current:  current,
		cursor:   0,
		selected: -1,
	}
}

func (m modelPickerModel) Init() tea.Cmd {
	return nil
}

func (m modelPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.models)-1 {
				m.cursor++
			}
		case "enter", " ":
			m.selected = m.cursor
			m.quitting = true
			return m, tea.Quit
		case "1", "2", "3", "4", "5", "6", "7", "8", "9":
			idx := int(msg.String()[0] - '1')
			if idx >= 0 && idx < len(m.models) {
				m.selected = idx
				m.quitting = true
				return m, tea.Quit
			}
		case "0":
			// Keep current - no change
			m.quitting = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m modelPickerModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Title
	b.WriteString("\n")
	b.WriteString(boldStyle.Render(m.title))
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("Current: " + m.current))
	b.WriteString("\n\n")

	// Models
	for i, model := range m.models {
		cursor := "  "
		style := unselectedStyle

		if i == m.cursor {
			cursor = cursorStyle.Render("> ")
			style = selectedStyle
		}

		// Show if this is the current model
		indicator := ""
		if model == m.current {
			indicator = successStyle.Render(" (current)")
		}

		number := mutedStyle.Render(fmt.Sprintf("%d. ", i+1))
		b.WriteString(fmt.Sprintf("%s%s%s%s\n", cursor, number, style.Render(model), indicator))
	}

	// Help
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("↑/↓ navigate • enter select • 0 keep current • q cancel"))
	b.WriteString("\n")

	return b.String()
}

// RunModelPicker runs an interactive model picker and returns the selected model ("" if canceled or kept current)
func RunModelPicker(title string, models []string, current string) string {
	if len(models) == 0 {
		return ""
	}
	m := newModelPickerModel(title, models, current)
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return ""
	}
	fm := finalModel.(modelPickerModel)
	if fm.selected >= 0 && fm.selected < len(fm.models) {
		return fm.models[fm.selected]
	}
	return ""
}
