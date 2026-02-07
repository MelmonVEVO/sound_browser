package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/speaker"
	"github.com/gopxl/beep/v2/vorbis"
	"github.com/gopxl/beep/v2/wav"
)

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#7D56F4")).
			MarginBottom(1)

	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#7D56F4")).
			Bold(true).
			PaddingLeft(2).
			PaddingRight(2)

	itemStyle = lipgloss.NewStyle().
			PaddingLeft(2)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#626262")).
			MarginTop(1)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF0000")).
			Bold(true)

	playingStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#00FF00"))
)

type keyMap struct {
	Up   key.Binding
	Down key.Binding
	Quit key.Binding
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("↑/k", "up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("↓/j", "down"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c", "esc"),
		key.WithHelp("q", "quit"),
	),
}

type audioFile struct {
	name string
	path string
}

type model struct {
	files                 []audioFile
	cursor                int
	termOffset            int
	termWidth, termHeight int
	currentCtrl           *beep.Ctrl
	currentStream         beep.StreamSeekCloser
	err                   string
	directory             string
}

func (m *model) listHeight() int {
	// Reserve space for:
	// title line(s) + blank lines + help + optional error.
	// Keep it simple and conservative.
	reserved := 6
	if m.err != "" {
		reserved += 2
	}
	h := m.termHeight - reserved
	if h < 1 {
		h = 1
	}
	return h
}

func (m *model) ensureCursorVisible() {
	h := m.listHeight()
	if m.cursor < m.termOffset {
		m.termOffset = m.cursor
	} else if m.cursor >= m.termOffset+h {
		m.termOffset = m.cursor - h + 1
	}
	m.clampOffset()
}

func (m *model) clampOffset() {
	max := len(m.files) - m.listHeight()
	if max < 0 {
		max = 0
	}
	if m.termOffset < 0 {
		m.termOffset = 0
	} else if m.termOffset > max {
		m.termOffset = max
	}
}

func initialModel(dir string) (model, error) {
	files, err := findAudioFiles(dir)
	if err != nil {
		return model{}, err
	}

	if len(files) == 0 {
		return model{}, fmt.Errorf("no audio files found in directory")
	}

	// Initialize speaker with a default sample rate
	// We'll reinitialize if needed when playing files
	sr := beep.SampleRate(44100)
	speaker.Init(sr, sr.N(time.Second/10))

	return model{
		files:     files,
		directory: dir,
	}, nil
}

func findAudioFiles(dir string) ([]audioFile, error) {
	var files []audioFile
	supportedExts := map[string]bool{
		".wav": true,
		".ogg": true,
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if supportedExts[ext] {
			files = append(files, audioFile{
				name: entry.Name(),
				path: filepath.Join(dir, entry.Name()),
			})
		}
	}

	return files, nil
}

func (m *model) Init() tea.Cmd {
	// Play the first file on startup
	if len(m.files) > 0 {
		return playAudio(m, m.files[0].path)
	}
	return nil
}

type audioFinishedMsg struct{}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termWidth = msg.Width
		m.termHeight = msg.Height
		m.clampOffset()
		return m, nil

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, keys.Quit):
			stopAudio(m)
			return m, tea.Quit

		case key.Matches(msg, keys.Up):
			if m.cursor > 0 {
				m.cursor--
				m.ensureCursorVisible()
				return m, playAudio(m, m.files[m.cursor].path)
			}
			return m, nil

		case key.Matches(msg, keys.Down):
			if m.cursor < len(m.files)-1 {
				m.cursor++
				m.ensureCursorVisible()
				return m, playAudio(m, m.files[m.cursor].path)
			}
			return m, nil
		}
		// ...
	}
	return m, nil
}

func stopAudio(m *model) {
	if m.currentCtrl != nil {
		speaker.Clear()
		m.currentCtrl = nil
	}
	if m.currentStream != nil {
		m.currentStream.Close()
		m.currentStream = nil
	}
}

func playAudio(m *model, path string) tea.Cmd {
	stopAudio(m)
	m.err = ""

	f, err := os.Open(path)
	if err != nil {
		m.err = fmt.Sprintf("Error opening file: %v", err)
		return nil
	}

	var streamer beep.StreamSeekCloser
	var format beep.Format

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".wav":
		streamer, format, err = wav.Decode(f)
	case ".ogg":
		streamer, format, err = vorbis.Decode(f)
	default:
		f.Close()
		m.err = "Unsupported file format"
		return nil
	}

	if err != nil {
		f.Close()
		m.err = fmt.Sprintf("Error decoding: %v", err)
		return nil
	}

	// Reinitialize speaker if sample rate changed
	speaker.Init(format.SampleRate, format.SampleRate.N(time.Second/10))

	m.currentStream = streamer

	// Play once (no loop)
	ctrl := &beep.Ctrl{Streamer: streamer}
	m.currentCtrl = ctrl

	done := make(chan bool)
	speaker.Play(beep.Seq(ctrl, beep.Callback(func() {
		close(done)
	})))

	return func() tea.Msg {
		<-done
		return audioFinishedMsg{}
	}
}

func (m *model) View() string {
	s := titleStyle.Render(fmt.Sprintf("🎵 Sound Browser - %s", m.directory))
	s += "\n\n"

	h := m.listHeight()
	start := m.termOffset
	end := start + h
	if end > len(m.files) {
		end = len(m.files)
	}

	for i := start; i < end; i++ {
		file := m.files[i]
		cursor := " "
		style := itemStyle

		if i == m.cursor {
			cursor = "▶"
			style = selectedStyle
		}

		s += fmt.Sprintf("%s %s\n", cursor, style.Render(file.name))
	}

	// Optional: show a little scroll indicator
	if len(m.files) > h {
		s += helpStyle.Render(fmt.Sprintf("Showing %d–%d of %d", start+1, end, len(m.files)))
		s += "\n"
	}

	if m.err != "" {
		s += "\n" + errorStyle.Render(m.err) + "\n"
	}

	s += "\n" + helpStyle.Render("↑/↓: navigate • q: quit")
	return s
}

func main() {
	dir := "."
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Printf("Error resolving directory: %v\n", err)
		os.Exit(1)
	}

	m, err := initialModel(absDir)
	if err != nil {
		fmt.Printf("Error initializing: %v\n", err)
		os.Exit(1)
	}

	p := tea.NewProgram(&m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running program: %v\n", err)
		os.Exit(1)
	}
}
