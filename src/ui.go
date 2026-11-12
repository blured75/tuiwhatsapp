package main

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type (
	qrMsg        string
	connMsg      string
	statusMsg    string
	refreshMsg   struct{}
	loggedOutMsg struct{}
)

type focus int

const (
	focusChats focus = iota
	focusInput
	focusMsgs
	focusAttach
)

const historyLimit = 500

var (
	green  = lipgloss.Color("#25D366")
	teal   = lipgloss.Color("#128C7E")
	grey   = lipgloss.Color("243")
	dim    = lipgloss.NewStyle().Foreground(grey)
	bold   = lipgloss.NewStyle().Bold(true)
	accent = lipgloss.NewStyle().Foreground(green).Bold(true)
	errSty = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))

	senderColors = []lipgloss.Color{"39", "208", "170", "114", "220", "81", "204", "147"}
)

type model struct {
	wa *WA
	st *Store

	w, h     int
	loggedIn bool
	qr       string
	conn     string
	status   string

	chats     []Chat
	visible   []Chat
	sel       int
	searching bool

	open    *Chat
	msgs    []Msg
	msgSel  int
	offsets []int
	heights []int
	thumbs  map[string]string

	vp     viewport.Model
	input  textinput.Model
	attach textinput.Model
	search textinput.Model
	focus  focus
	busy   int
}

func newModel(wa *WA, st *Store) *model {
	in := textinput.New()
	in.Placeholder = "Écrire un message…"
	in.Prompt = "› "
	in.CharLimit = 0
	at := textinput.New()
	at.Placeholder = "~/Images/photo.jpg  (Tab complète le chemin)"
	at.Prompt = "Fichier › "
	at.CharLimit = 0
	se := textinput.New()
	se.Placeholder = "rechercher"
	se.Prompt = "/ "
	m := &model{
		wa: wa, st: st,
		loggedIn: wa.LoggedIn(),
		conn:     "connexion…",
		thumbs:   map[string]string{},
		vp:       viewport.New(0, 0),
		input:    in, attach: at, search: se,
	}
	m.chats, _ = st.Chats()
	m.applyFilter()
	return m
}

func (m *model) Init() tea.Cmd { return textinput.Blink }

// ---------- layout ----------

func (m *model) leftW() int { return min(max(m.w/3, 24), 40) }

func (m *model) layout() {
	rw := m.w - m.leftW()
	m.vp.Width = max(rw-2, 10)
	m.vp.Height = max(m.h-1-2-3, 1)
	m.input.Width = max(rw-8, 5)
	m.attach.Width = max(rw-16, 5)
	m.search.Width = max(m.leftW()-6, 5)
	m.thumbs = map[string]string{}
	m.rebuild()
}

// ---------- data ----------

func (m *model) applyFilter() {
	var cur string
	if m.sel < len(m.visible) {
		cur = m.visible[m.sel].JID
	}
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	m.visible = m.visible[:0]
	for _, c := range m.chats {
		if q == "" || strings.Contains(strings.ToLower(c.Title()), q) {
			m.visible = append(m.visible, c)
		}
	}
	m.sel = 0
	for i, c := range m.visible {
		if c.JID == cur {
			m.sel = i
		}
	}
}

func (m *model) reload() tea.Cmd {
	m.chats, _ = m.st.Chats()
	m.applyFilter()
	if m.open == nil {
		return nil
	}
	var unread int
	for _, c := range m.chats {
		if c.JID == m.open.JID {
			unread = c.Unread
			cc := c
			m.open = &cc
		}
	}
	atBottom := m.vp.AtBottom()
	onLast := m.msgSel >= len(m.msgs)-1
	m.msgs, _ = m.st.Messages(m.open.JID, historyLimit)
	if onLast || m.msgSel >= len(m.msgs) {
		m.msgSel = len(m.msgs) - 1
	}
	m.rebuild()
	if atBottom {
		m.vp.GotoBottom()
	}
	if unread > 0 {
		return m.markRead(m.open.JID, unread)
	}
	return nil
}

func (m *model) markRead(jid string, unread int) tea.Cmd {
	m.st.ClearUnread(jid)
	for i := range m.chats {
		if m.chats[i].JID == jid {
			m.chats[i].Unread = 0
		}
	}
	for i := range m.visible {
		if m.visible[i].JID == jid {
			m.visible[i].Unread = 0
		}
	}
	wa := m.wa
	return func() tea.Msg { wa.MarkRead(jid, unread); return nil }
}

func (m *model) openChat() tea.Cmd {
	if m.sel >= len(m.visible) {
		return nil
	}
	c := m.visible[m.sel]
	m.open = &c
	m.msgs, _ = m.st.Messages(c.JID, historyLimit)
	m.msgSel = len(m.msgs) - 1
	m.thumbs = map[string]string{}
	m.setFocus(focusInput)
	m.rebuild()
	m.vp.GotoBottom()
	if c.Unread > 0 {
		return m.markRead(c.JID, c.Unread)
	}
	return nil
}

func (m *model) setFocus(f focus) {
	m.focus = f
	m.input.Blur()
	m.attach.Blur()
	switch f {
	case focusInput:
		m.input.Focus()
	case focusAttach:
		m.attach.Focus()
	}
	m.rebuild()
}

// ---------- update ----------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layout()
		m.vp.GotoBottom()
		return m, nil
	case qrMsg:
		m.qr = string(msg)
		m.loggedIn = false
		return m, nil
	case connMsg:
		m.conn = string(msg)
		if m.conn == "connecté" {
			m.loggedIn = true
			m.qr = ""
		}
		return m, nil
	case loggedOutMsg:
		m.loggedIn = false
		m.status = "Session fermée depuis le téléphone — scannez à nouveau le QR code."
		return m, nil
	case statusMsg:
		m.status = string(msg)
		return m, nil
	case sentMsg:
		m.busy--
		if msg.err != nil {
			m.status = "Échec de l'envoi : " + msg.err.Error()
		} else if msg.note != "" {
			m.status = msg.note
		}
		return m, m.reload()
	case openedMsg:
		m.busy--
		if msg.err != nil {
			m.status = "Média indisponible : " + msg.err.Error()
		} else {
			m.status = "Ouvert : " + msg.path
		}
		return m, m.reload()
	case refreshMsg:
		return m, m.reload()
	case tea.MouseMsg:
		if m.loggedIn && m.open != nil {
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
		return m, nil
	case tea.KeyMsg:
		return m.onKey(msg)
	}
	var cmd tea.Cmd
	switch m.focus {
	case focusInput:
		m.input, cmd = m.input.Update(msg)
	case focusAttach:
		m.attach, cmd = m.attach.Update(msg)
	case focusChats:
		m.search, cmd = m.search.Update(msg)
	}
	return m, cmd
}

type sentMsg struct {
	err  error
	note string
}

type openedMsg struct {
	path string
	err  error
}

func (m *model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	if !m.loggedIn {
		if k == "q" {
			return m, tea.Quit
		}
		return m, nil
	}
	var cmd tea.Cmd
	switch m.focus {
	case focusChats:
		if m.searching {
			switch k {
			case "esc":
				m.searching = false
				m.search.SetValue("")
				m.search.Blur()
				m.applyFilter()
			case "enter":
				m.searching = false
				m.search.Blur()
				return m, m.openChat()
			case "up":
				m.sel = max(m.sel-1, 0)
			case "down":
				m.sel = min(m.sel+1, max(len(m.visible)-1, 0))
			default:
				m.search, cmd = m.search.Update(msg)
				m.applyFilter()
			}
			return m, cmd
		}
		switch k {
		case "q":
			return m, tea.Quit
		case "up", "k":
			m.sel = max(m.sel-1, 0)
		case "down", "j":
			m.sel = min(m.sel+1, max(len(m.visible)-1, 0))
		case "pgup":
			m.sel = max(m.sel-10, 0)
		case "pgdown":
			m.sel = min(m.sel+10, max(len(m.visible)-1, 0))
		case "home", "g":
			m.sel = 0
		case "end", "G":
			m.sel = max(len(m.visible)-1, 0)
		case "/":
			m.searching = true
			m.search.Focus()
			return m, textinput.Blink
		case "esc":
			m.search.SetValue("")
			m.applyFilter()
		case "enter", "right", "l":
			return m, m.openChat()
		case "tab":
			if m.open != nil {
				m.setFocus(focusInput)
			} else {
				return m, m.openChat()
			}
		}
		return m, nil

	case focusInput:
		switch k {
		case "esc":
			m.setFocus(focusChats)
		case "tab", "up":
			if len(m.msgs) > 0 {
				m.msgSel = min(max(m.msgSel, 0), len(m.msgs)-1)
				m.setFocus(focusMsgs)
				m.scrollToSel()
			}
		case "pgup":
			m.vp.HalfPageUp()
		case "pgdown":
			m.vp.HalfPageDown()
		case "ctrl+o":
			m.setFocus(focusAttach)
			return m, textinput.Blink
		case "enter":
			text := strings.TrimSpace(m.input.Value())
			if text == "" || m.open == nil {
				return m, nil
			}
			m.input.SetValue("")
			m.busy++
			wa, jid := m.wa, m.open.JID
			return m, func() tea.Msg { return sentMsg{err: wa.SendText(jid, text)} }
		default:
			m.input, cmd = m.input.Update(msg)
		}
		return m, cmd

	case focusMsgs:
		switch k {
		case "esc", "tab", "i":
			m.setFocus(focusInput)
			m.vp.GotoBottom()
		case "up", "k":
			m.msgSel = max(m.msgSel-1, 0)
			m.rebuild()
			m.scrollToSel()
		case "down", "j":
			if m.msgSel >= len(m.msgs)-1 {
				m.setFocus(focusInput)
				m.vp.GotoBottom()
				break
			}
			m.msgSel++
			m.rebuild()
			m.scrollToSel()
		case "home", "g":
			m.msgSel = 0
			m.rebuild()
			m.scrollToSel()
		case "end", "G":
			m.msgSel = len(m.msgs) - 1
			m.rebuild()
			m.scrollToSel()
		case "pgup":
			m.vp.HalfPageUp()
		case "pgdown":
			m.vp.HalfPageDown()
		case "y", "c":
			if m.msgSel < 0 || m.msgSel >= len(m.msgs) {
				return m, nil
			}
			text := m.msgs[m.msgSel].Body
			if m.msgs[m.msgSel].Kind == "audio" || strings.TrimSpace(text) == "" {
				m.status = "Ce message ne contient pas de texte."
				return m, nil
			}
			if err := copyText(text); err != nil {
				m.status = "Copie impossible : " + err.Error()
			} else {
				m.status = "Message copié."
			}
			return m, nil
		case "enter", "o":
			if m.msgSel < 0 || m.msgSel >= len(m.msgs) {
				return m, nil
			}
			sel := m.msgs[m.msgSel]
			if !sel.IsMedia() {
				m.status = "Ce message ne contient pas de média."
				return m, nil
			}
			m.status = "Téléchargement…"
			m.busy++
			wa := m.wa
			return m, func() tea.Msg {
				path, err := wa.Download(sel)
				if err == nil {
					err = openExternal(path)
				}
				return openedMsg{path: path, err: err}
			}
		}
		return m, nil

	case focusAttach:
		switch k {
		case "esc":
			m.attach.SetValue("")
			m.setFocus(focusInput)
		case "tab":
			val, cands := completePath(m.attach.Value())
			m.attach.SetValue(val)
			m.attach.CursorEnd()
			switch {
			case len(cands) == 0:
				m.status = "Aucun fichier ne correspond."
			case len(cands) > 1:
				m.status = strings.Join(cands, "  ")
			default:
				m.status = ""
			}
		case "enter":
			path := strings.TrimSpace(m.attach.Value())
			if path == "" || m.open == nil {
				return m, nil
			}
			caption := strings.TrimSpace(m.input.Value())
			m.attach.SetValue("")
			m.input.SetValue("")
			m.setFocus(focusInput)
			m.status = "Envoi du fichier…"
			m.busy++
			wa, jid := m.wa, m.open.JID
			return m, func() tea.Msg {
				if err := wa.SendFile(jid, path, caption); err != nil {
					return sentMsg{err: err}
				}
				return sentMsg{note: "Fichier envoyé."}
			}
		default:
			m.attach, cmd = m.attach.Update(msg)
		}
		return m, cmd
	}
	return m, nil
}

func (m *model) scrollToSel() {
	if m.msgSel < 0 || m.msgSel >= len(m.offsets) {
		return
	}
	top, h := m.offsets[m.msgSel], m.heights[m.msgSel]
	if top < m.vp.YOffset {
		m.vp.SetYOffset(top)
	} else if top+h > m.vp.YOffset+m.vp.Height {
		m.vp.SetYOffset(top + h - m.vp.Height)
	}
}

// ---------- message rendering ----------

func mediaLabel(msg Msg) string {
	switch msg.Kind {
	case "image":
		return "▣ Photo"
	case "video":
		return "▶ Vidéo"
	case "audio":
		return "♪ Audio " + msg.Body
	case "doc":
		return "▤ Document"
	case "sticker":
		return "◍ Sticker"
	}
	return ""
}

func (m *model) thumb(msg Msg) string {
	if len(msg.Thumb) == 0 {
		return ""
	}
	if t, ok := m.thumbs[msg.ID]; ok {
		return t
	}
	t := renderImage(msg.Thumb, min(36, m.vp.Width-8), 14)
	m.thumbs[msg.ID] = t
	return t
}

func (m *model) renderMsg(i int, msg Msg, maxw int, group bool) string {
	var parts []string
	if group && !msg.FromMe {
		h := fnv.New32a()
		h.Write([]byte(msg.Sender))
		c := senderColors[int(h.Sum32())%len(senderColors)]
		parts = append(parts, lipgloss.NewStyle().Foreground(c).Bold(true).Render(ansi.Truncate(msg.SenderName, maxw, "…")))
	}
	selected := m.focus == focusMsgs && i == m.msgSel
	if msg.IsMedia() {
		if t := m.thumb(msg); t != "" {
			parts = append(parts, t)
		}
		label := mediaLabel(msg)
		switch {
		case selected:
			label += dim.Render("  ↵ ouvrir")
		case msg.Path != "":
			label += dim.Render("  ✓ enregistré")
		}
		parts = append(parts, accent.Render(label))
	}
	body := msg.Body
	if msg.Kind == "audio" {
		body = ""
	}
	if body != "" {
		body = strings.ReplaceAll(body, "\t", "  ")
		wrapped := ansi.Wrap(body, maxw, "")
		if msg.Kind == "other" {
			wrapped = dim.Italic(true).Render(wrapped)
		}
		parts = append(parts, wrapped)
	}
	parts = append(parts, dim.Render(msg.TS.Local().Format("15:04")))

	border := lipgloss.Color("240")
	if msg.FromMe {
		border = teal
	}
	if selected {
		border = lipgloss.Color("#FFD75F")
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(border).Padding(0, 1).
		Render(strings.Join(parts, "\n"))
}

func dayLabel(t time.Time) string {
	now := time.Now()
	y, mo, d := t.Date()
	ny, nm, nd := now.Date()
	if y == ny && mo == nm && d == nd {
		return "Aujourd'hui"
	}
	yy, ym, yd := now.AddDate(0, 0, -1).Date()
	if y == yy && mo == ym && d == yd {
		return "Hier"
	}
	return t.Format("02/01/2006")
}

func (m *model) rebuild() {
	m.offsets, m.heights = m.offsets[:0], m.heights[:0]
	if m.open == nil || m.vp.Width <= 0 {
		m.vp.SetContent("")
		return
	}
	width := m.vp.Width
	maxw := min(width-6, 72)
	group := m.open.IsGroup()
	var blocks []string
	line, lastDay := 0, ""
	for i, msg := range m.msgs {
		ts := msg.TS.Local()
		if day := ts.Format("2006-01-02"); day != lastDay {
			lastDay = day
			blocks = append(blocks, lipgloss.PlaceHorizontal(width, lipgloss.Center, dim.Render("── "+dayLabel(ts)+" ──")))
			line++
		}
		block := m.renderMsg(i, msg, maxw, group)
		if msg.FromMe {
			block = lipgloss.PlaceHorizontal(width, lipgloss.Right, block)
		}
		h := lipgloss.Height(block)
		m.offsets = append(m.offsets, line)
		m.heights = append(m.heights, h)
		line += h
		blocks = append(blocks, block)
	}
	off := m.vp.YOffset
	m.vp.SetContent(strings.Join(blocks, "\n"))
	m.vp.SetYOffset(off)
}

// ---------- views ----------

func (m *model) View() string {
	if m.w == 0 {
		return ""
	}
	if !m.loggedIn {
		return m.loginView()
	}
	if m.w < 50 || m.h < 10 {
		return "Terminal trop petit."
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.chatsView(), m.chatView())
	return body + "\n" + m.statusView()
}

func (m *model) loginView() string {
	var b strings.Builder
	b.WriteString(accent.Render("WhatsApp TUI") + "\n\n")
	if m.qr == "" {
		b.WriteString("Connexion à WhatsApp…\n")
	} else {
		b.WriteString("Sur votre téléphone : WhatsApp › Appareils connectés › Connecter un appareil\n\n")
		b.WriteString(renderQR(m.qr) + "\n\n")
		b.WriteString(dim.Render("Le code se renouvelle automatiquement. Ctrl+C pour quitter."))
	}
	if m.status != "" {
		b.WriteString("\n" + errSty.Render(m.status))
	}
	return lipgloss.Place(m.w, m.h, lipgloss.Center, lipgloss.Center, b.String())
}

func shortTime(t time.Time) string {
	t = t.Local()
	now := time.Now()
	switch {
	case t.Unix() <= 0:
		return ""
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("15:04")
	case now.Sub(t) < 6*24*time.Hour:
		return [...]string{"dim.", "lun.", "mar.", "mer.", "jeu.", "ven.", "sam."}[t.Weekday()]
	}
	return t.Format("02/01/06")
}

func preview(c Chat) string {
	p := strings.ReplaceAll(c.LastBody, "\n", " ")
	if l := mediaLabel(Msg{Kind: c.LastKind, Body: c.LastBody}); l != "" {
		if c.LastKind == "audio" || p == "" {
			p = l
		} else {
			p = l + " " + p
		}
	}
	if c.LastMine {
		p = "Vous : " + p
	}
	return p
}

func (m *model) chatsView() string {
	inner := m.leftW() - 1
	height := m.h - 1
	conn := dim.Render("● " + m.conn)
	if m.conn == "connecté" {
		conn = accent.Render("●") + dim.Render(" connecté")
	}
	lines := []string{" " + accent.Render("WhatsApp") + "  " + conn}
	if m.searching || m.search.Value() != "" {
		lines = append(lines, " "+m.search.View())
	} else {
		lines = append(lines, dim.Render(" / pour rechercher"))
	}

	rows := max((height-len(lines))/2, 1)
	start := 0
	if m.sel >= rows {
		start = m.sel - rows + 1
	}
	if len(m.visible) == 0 {
		lines = append(lines, "", dim.Render(" Aucune discussion pour l'instant."), dim.Render(" L'historique arrive après la"), dim.Render(" première connexion."))
	}
	for i := start; i < len(m.visible) && i < start+rows; i++ {
		c := m.visible[i]
		mark := " "
		if m.open != nil && m.open.JID == c.JID {
			mark = accent.Render("▌")
		}
		ts := shortTime(c.LastTS)
		nameW := max(inner-2-len([]rune(ts))-1, 4)
		name := ansi.Truncate(c.Title(), nameW, "…")
		badge := ""
		if c.Unread > 0 {
			badge = fmt.Sprintf(" %d ", c.Unread)
		}
		prevW := max(inner-2-len(badge)-1, 4)
		prev := ansi.Truncate(preview(c), prevW, "…")

		nameSty, prevSty, tsSty := bold, dim, dim
		badgeSty := lipgloss.NewStyle().Background(green).Foreground(lipgloss.Color("0")).Bold(true)
		if c.Unread > 0 {
			tsSty = accent
		}
		if i == m.sel {
			bg := lipgloss.Color("237")
			if m.focus == focusChats {
				bg = lipgloss.Color("24")
			}
			nameSty, prevSty, tsSty = nameSty.Background(bg), lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Background(bg), tsSty.Background(bg)
			fill := lipgloss.NewStyle().Background(bg)
			l1 := mark + fill.Render(" ") + nameSty.Render(name) + fill.Render(strings.Repeat(" ", max(inner-2-ansi.StringWidth(name)-len([]rune(ts)), 0))) + tsSty.Render(ts)
			l2 := mark + fill.Render(" ") + prevSty.Render(prev) + fill.Render(strings.Repeat(" ", max(inner-2-ansi.StringWidth(prev)-len(badge), 0))) + badgeSty.Render(badge)
			lines = append(lines, l1, l2)
			continue
		}
		l1 := mark + " " + nameSty.Render(name) + strings.Repeat(" ", max(inner-2-ansi.StringWidth(name)-len([]rune(ts)), 0)) + tsSty.Render(ts)
		l2 := mark + " " + prevSty.Render(prev) + strings.Repeat(" ", max(inner-2-ansi.StringWidth(prev)-len(badge), 0)) + badgeSty.Render(badge)
		lines = append(lines, l1, l2)
	}
	return lipgloss.NewStyle().Width(inner).Height(height).MaxHeight(height).MaxWidth(inner+1).
		Border(lipgloss.NormalBorder(), false, true, false, false).BorderForeground(lipgloss.Color("238")).
		Render(strings.Join(lines, "\n"))
}

func (m *model) chatView() string {
	rw := m.w - m.leftW()
	height := m.h - 1
	if m.open == nil {
		hint := accent.Render("WhatsApp TUI") + "\n\n" + dim.Render("Choisissez une discussion avec ↑/↓ puis Entrée.")
		hint = lipgloss.NewStyle().Width(max(rw-4, 1)).Align(lipgloss.Center).Render(hint)
		return lipgloss.Place(rw, height, lipgloss.Center, lipgloss.Center, hint)
	}
	suffix := ""
	if m.open.IsGroup() {
		suffix = "  groupe"
	}
	title := " " + bold.Render(ansi.Truncate(m.open.Title(), max(rw-3-len(suffix), 4), "…")) + dim.Render(suffix)
	header := lipgloss.NewStyle().Width(rw).MaxHeight(2).Border(lipgloss.NormalBorder(), false, false, true, false).
		BorderForeground(lipgloss.Color("238")).Render(title)

	body := lipgloss.NewStyle().Padding(0, 1).Render(m.vp.View())
	if len(m.msgs) == 0 {
		body = lipgloss.Place(rw, m.vp.Height, lipgloss.Center, lipgloss.Center, dim.Render("Aucun message."))
	}

	field, bc := m.input.View(), lipgloss.Color("238")
	switch m.focus {
	case focusAttach:
		field, bc = m.attach.View(), lipgloss.Color("#FFD75F")
	case focusInput:
		bc = green
	}
	box := lipgloss.NewStyle().Width(rw-2).Border(lipgloss.RoundedBorder()).BorderForeground(bc).
		Padding(0, 1).MaxHeight(3).Render(field)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, box)
}

func (m *model) statusView() string {
	var help string
	switch m.focus {
	case focusChats:
		help = "↑↓ naviguer · ↵ ouvrir · / chercher · q quitter"
		if m.searching {
			help = "↵ ouvrir · Échap annuler"
		}
	case focusInput:
		help = "↵ envoyer · Ctrl+O joindre photo/vidéo · Tab messages · Échap discussions"
	case focusMsgs:
		help = "↑↓ sélectionner · y copier · ↵ ouvrir le média · Échap retour"
	case focusAttach:
		help = "↵ envoyer le fichier · Tab compléter · Échap annuler"
	}
	left := m.status
	if m.busy > 0 && left == "" {
		left = "Envoi…"
	}
	left = ansi.Truncate(strings.ReplaceAll(left, "\n", " "), max(m.w-2, 1), "…")
	room := m.w - ansi.StringWidth(left) - 3
	right := ""
	if room > 10 {
		right = dim.Render(ansi.Truncate(help, room, "…"))
	}
	gap := max(m.w-1-ansi.StringWidth(left)-ansi.StringWidth(right)-1, 1)
	return " " + left + strings.Repeat(" ", gap) + right
}
