package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// WA wraps the WhatsApp client and feeds the local store + the UI.
type WA struct {
	cli      *whatsmeow.Client
	st       *Store
	ctx      context.Context
	send     func(tea.Msg)
	mediaDir string

	mu       sync.Mutex
	timer    *time.Timer
	names    map[string]string
	fetching map[string]bool
}

func NewWA(ctx context.Context, dataDir string, st *Store) (*WA, error) {
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(dataDir, "session.db")))
	if err != nil {
		return nil, err
	}
	container := sqlstore.NewWithDB(db, "sqlite3", nil)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("base de session: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, err
	}
	store.DeviceProps.Os = proto.String("TUI WhatsApp")

	mediaDir := filepath.Join(dataDir, "media")
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		return nil, err
	}
	w := &WA{
		cli:      whatsmeow.NewClient(device, nil),
		st:       st,
		ctx:      ctx,
		send:     func(tea.Msg) {},
		mediaDir: mediaDir,
		names:    map[string]string{},
		fetching: map[string]bool{},
	}
	w.cli.AddEventHandler(w.handle)
	return w, nil
}

func (w *WA) LoggedIn() bool { return w.cli.Store.ID != nil }

// Start connects, going through QR pairing first when there is no session yet.
func (w *WA) Start() {
	if w.LoggedIn() {
		w.send(connMsg("connexion…"))
		if err := w.cli.Connect(); err != nil {
			w.send(statusMsg("Connexion impossible : " + err.Error()))
		}
		return
	}
	for w.ctx.Err() == nil {
		qrChan, err := w.cli.GetQRChannel(w.ctx)
		if err != nil {
			w.send(statusMsg("QR : " + err.Error()))
			return
		}
		if err := w.cli.Connect(); err != nil {
			w.send(statusMsg("Connexion impossible : " + err.Error() + " — nouvel essai…"))
			time.Sleep(3 * time.Second)
			continue
		}
		paired := false
		for item := range qrChan {
			switch item.Event {
			case "code":
				w.send(qrMsg(item.Code))
			case "success":
				paired = true
			case "error":
				w.send(statusMsg(fmt.Sprintf("Appairage : %v", item.Error)))
			}
		}
		if paired {
			return
		}
		w.cli.Disconnect()
		time.Sleep(time.Second)
	}
}

func (w *WA) Stop() { w.cli.Disconnect() }

func (w *WA) refresh() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		return
	}
	w.timer = time.AfterFunc(120*time.Millisecond, func() {
		w.mu.Lock()
		w.timer = nil
		w.mu.Unlock()
		w.send(refreshMsg{})
	})
}

// norm maps device/LID addresses to the plain phone-number JID so one contact = one chat.
func (w *WA) norm(j types.JID) types.JID {
	j = j.ToNonAD()
	if j.Server == types.HiddenUserServer {
		if pn, err := w.cli.Store.LIDs.GetPNForLID(w.ctx, j); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD()
		}
	}
	return j
}

func pickName(c types.ContactInfo) string {
	for _, n := range []string{c.FullName, c.FirstName, c.BusinessName, c.PushName} {
		if n != "" {
			return n
		}
	}
	return ""
}

// contactName returns the best known name for a user, or "".
func (w *WA) contactName(j types.JID) string {
	key := j.String()
	w.mu.Lock()
	name, ok := w.names[key]
	w.mu.Unlock()
	if ok {
		return name
	}
	if w.cli.Store.ID != nil && j.User == w.cli.Store.ID.User {
		name = "Moi (notes)"
	}
	if name == "" {
		if c, err := w.cli.Store.Contacts.GetContact(w.ctx, j); err == nil && c.Found {
			name = pickName(c)
		}
	}
	if name == "" && j.Server == types.DefaultUserServer {
		if lid, err := w.cli.Store.LIDs.GetLIDForPN(w.ctx, j); err == nil && !lid.IsEmpty() {
			if c, err := w.cli.Store.Contacts.GetContact(w.ctx, lid); err == nil && c.Found {
				name = pickName(c)
			}
		}
	}
	w.mu.Lock()
	w.names[key] = name
	w.mu.Unlock()
	return name
}

func (w *WA) forgetNames() {
	w.mu.Lock()
	w.names = map[string]string{}
	w.mu.Unlock()
}

func (w *WA) fetchGroupName(j types.JID) {
	key := j.String()
	w.mu.Lock()
	if w.fetching[key] {
		w.mu.Unlock()
		return
	}
	w.fetching[key] = true
	w.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(w.ctx, 20*time.Second)
		defer cancel()
		if info, err := w.cli.GetGroupInfo(ctx, j); err == nil && info.Name != "" {
			w.st.SetChatName(key, info.Name)
			w.refresh()
		} else {
			w.mu.Lock()
			delete(w.fetching, key)
			w.mu.Unlock()
		}
	}()
}

// syncNames refreshes chat titles from the contact list and group metadata.
func (w *WA) syncNames() {
	w.forgetNames()
	for _, js := range w.st.ChatJIDs() {
		j, err := types.ParseJID(js)
		if err != nil || j.Server == types.GroupServer {
			continue
		}
		w.st.SetChatName(js, w.contactName(j))
	}
	w.refresh()
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()
	if groups, err := w.cli.GetJoinedGroups(ctx); err == nil {
		for _, g := range groups {
			w.st.SetChatName(g.JID.String(), g.Name)
		}
		w.refresh()
	}
}

func (w *WA) handle(raw any) {
	switch evt := raw.(type) {
	case *events.Connected:
		w.send(connMsg("connecté"))
		go w.syncNames()
	case *events.Disconnected:
		w.send(connMsg("déconnecté"))
	case *events.StreamReplaced:
		w.send(connMsg("session ouverte ailleurs"))
	case *events.LoggedOut:
		w.send(loggedOutMsg{})
		go w.Start()
	case *events.PairSuccess:
		w.send(statusMsg("Appareil lié, synchronisation de l'historique…"))
	case *events.Contact:
		w.forgetNames()
		w.st.SetChatName(w.norm(evt.JID).String(), w.contactName(w.norm(evt.JID)))
		w.refresh()
	case *events.PushName:
		w.forgetNames()
		w.st.SetChatName(w.norm(evt.JID).String(), w.contactName(w.norm(evt.JID)))
		w.refresh()
	case *events.Message:
		w.onMessage(evt)
	case *events.HistorySync:
		w.onHistory(evt)
	}
}

func (w *WA) onMessage(evt *events.Message) {
	m, ok := w.convert(evt)
	if !ok {
		w.refresh() // edits/deletions change the store without producing a message
		return
	}
	n, err := w.st.InsertMsgs([]Msg{m})
	if err != nil || n == 0 {
		return
	}
	chat, _ := types.ParseJID(m.Chat)
	name, unread := "", 0
	if !m.FromMe {
		unread = 1
	}
	if chat.Server == types.GroupServer {
		if w.st.ChatName(m.Chat) == "" {
			w.fetchGroupName(chat)
		}
	} else {
		name = w.contactName(chat)
		if name == "" && !m.FromMe {
			name = evt.Info.PushName
		}
	}
	w.st.UpsertChat(m.Chat, name, m.TS, unread)
	w.refresh()
}

func (w *WA) onHistory(evt *events.HistorySync) {
	for _, conv := range evt.Data.GetConversations() {
		jid, err := types.ParseJID(conv.GetID())
		if err != nil {
			continue
		}
		chat := w.norm(jid)
		if chat.Server != types.DefaultUserServer && chat.Server != types.GroupServer && chat.Server != types.HiddenUserServer {
			continue
		}
		var batch []Msg
		var last time.Time
		for _, hm := range conv.GetMessages() {
			e, err := w.cli.ParseWebMessage(jid, hm.GetMessage())
			if err != nil {
				continue
			}
			if m, ok := w.convert(e); ok {
				batch = append(batch, m)
				if m.TS.After(last) {
					last = m.TS
				}
			}
		}
		if len(batch) == 0 {
			continue
		}
		if _, err := w.st.InsertMsgs(batch); err != nil {
			continue
		}
		name := ""
		if chat.Server == types.GroupServer {
			name = conv.GetName()
		} else if name = w.contactName(chat); name == "" {
			name = conv.GetDisplayName()
		}
		w.st.UpsertChat(chat.String(), name, last, 0)
	}
	w.refresh()
}

// describe extracts what we display from a message. kind == "" means "nothing to show".
// media is a minimal message holding only the downloadable part.
func describe(m *waE2E.Message) (kind, body string, thumb []byte, media *waE2E.Message) {
	switch {
	case m == nil:
	case m.GetConversation() != "":
		return "text", m.GetConversation(), nil, nil
	case m.GetExtendedTextMessage() != nil:
		return "text", m.GetExtendedTextMessage().GetText(), nil, nil
	case m.GetImageMessage() != nil:
		im := m.GetImageMessage()
		return "image", im.GetCaption(), im.GetJPEGThumbnail(), &waE2E.Message{ImageMessage: im}
	case m.GetVideoMessage() != nil:
		vm := m.GetVideoMessage()
		return "video", vm.GetCaption(), vm.GetJPEGThumbnail(), &waE2E.Message{VideoMessage: vm}
	case m.GetPtvMessage() != nil:
		vm := m.GetPtvMessage()
		return "video", vm.GetCaption(), vm.GetJPEGThumbnail(), &waE2E.Message{VideoMessage: vm}
	case m.GetAudioMessage() != nil:
		am := m.GetAudioMessage()
		body = fmt.Sprintf("%d:%02d", am.GetSeconds()/60, am.GetSeconds()%60)
		return "audio", body, nil, &waE2E.Message{AudioMessage: am}
	case m.GetDocumentMessage() != nil:
		dm := m.GetDocumentMessage()
		body = dm.GetFileName()
		if body == "" {
			body = dm.GetTitle()
		}
		if c := dm.GetCaption(); c != "" && c != body {
			body += "\n" + c
		}
		return "doc", body, dm.GetJPEGThumbnail(), &waE2E.Message{DocumentMessage: dm}
	case m.GetStickerMessage() != nil:
		return "sticker", "", nil, &waE2E.Message{StickerMessage: m.GetStickerMessage()}
	case m.GetLocationMessage() != nil:
		l := m.GetLocationMessage()
		return "other", fmt.Sprintf("Localisation : %.5f, %.5f %s", l.GetDegreesLatitude(), l.GetDegreesLongitude(), l.GetName()), nil, nil
	case m.GetContactMessage() != nil:
		return "other", "Contact : " + m.GetContactMessage().GetDisplayName(), nil, nil
	case m.GetPollCreationMessage() != nil:
		return "other", "Sondage : " + m.GetPollCreationMessage().GetName(), nil, nil
	}
	return "", "", nil, nil
}

func (w *WA) convert(evt *events.Message) (Msg, bool) {
	chat := w.norm(evt.Info.Chat)
	if chat.Server == types.BroadcastServer || chat.Server == types.NewsletterServer || evt.Message == nil {
		return Msg{}, false
	}
	if pm := evt.Message.GetProtocolMessage(); pm != nil {
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			w.st.UpdateBody(chat.String(), pm.GetKey().GetID(), "other", "Message supprimé")
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if kind, body, _, _ := describe(pm.GetEditedMessage()); kind != "" {
				w.st.EditBody(chat.String(), pm.GetKey().GetID(), body)
			}
		}
		return Msg{}, false
	}
	kind, body, thumb, media := describe(evt.Message)
	if kind == "" {
		return Msg{}, false
	}
	sender := w.norm(evt.Info.Sender)
	m := Msg{
		Chat:   chat.String(),
		ID:     evt.Info.ID,
		Sender: sender.String(),
		FromMe: evt.Info.IsFromMe,
		TS:     evt.Info.Timestamp,
		Kind:   kind,
		Body:   body,
		Thumb:  thumb,
	}
	if m.TS.IsZero() {
		m.TS = time.Now()
	}
	if media != nil {
		m.Raw, _ = proto.Marshal(media)
	}
	if !m.FromMe {
		if m.SenderName = w.contactName(sender); m.SenderName == "" {
			m.SenderName = evt.Info.PushName
		}
		if m.SenderName == "" {
			m.SenderName = "+" + sender.User
		}
	}
	return m, true
}

func (w *WA) recordSent(chat types.JID, resp whatsmeow.SendResponse, m Msg) {
	m.Chat = chat.String()
	m.ID = resp.ID
	m.FromMe = true
	m.TS = resp.Timestamp
	if m.TS.IsZero() {
		m.TS = time.Now()
	}
	if w.cli.Store.ID != nil {
		m.Sender = w.cli.Store.ID.ToNonAD().String()
	}
	w.st.InsertMsgs([]Msg{m})
	w.st.UpsertChat(m.Chat, "", m.TS, 0)
	w.refresh()
}

func (w *WA) SendText(chat, text string) error {
	jid, err := types.ParseJID(chat)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(w.ctx, 60*time.Second)
	defer cancel()
	resp, err := w.cli.SendMessage(ctx, jid, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return err
	}
	w.recordSent(jid, resp, Msg{Kind: "text", Body: text})
	return nil
}

// SendFile uploads a file and sends it as photo, video, audio or document depending on its type.
func (w *WA) SendFile(chat, path, caption string) error {
	jid, err := types.ParseJID(chat)
	if err != nil {
		return err
	}
	path = expandPath(path)
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("c'est un dossier")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	ext := strings.ToLower(filepath.Ext(path))
	mimeType := mime.TypeByExtension(ext)
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	mimeType, _, _ = strings.Cut(mimeType, ";")
	_, ffErr := exec.LookPath("ffmpeg")
	hasFF := ffErr == nil

	ctx, cancel := context.WithTimeout(w.ctx, 15*time.Minute)
	defer cancel()
	var pm *waE2E.Message
	rec := Msg{Body: caption, Path: path}
	var capPtr *string
	if caption != "" {
		capPtr = proto.String(caption)
	}

	kind := "doc"
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		kind = "image"
		if mimeType != "image/jpeg" && mimeType != "image/png" {
			if conv, err := toJPEG(data); err == nil {
				data, mimeType = conv, "image/jpeg"
			} else {
				kind = "doc"
			}
		}
	case strings.HasPrefix(mimeType, "video/"):
		if hasFF {
			kind = "video"
		} else if mimeType == "video/mp4" {
			kind = "video"
		}
	case strings.HasPrefix(mimeType, "audio/"):
		kind = "audio"
	}

	switch kind {
	case "image":
		thumb, iw, ih := makeThumb(data)
		up, err := w.cli.Upload(ctx, data, whatsmeow.MediaImage)
		if err != nil {
			return err
		}
		pm = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption: capPtr, Mimetype: proto.String(mimeType),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), JPEGThumbnail: thumb,
			Width: proto.Uint32(uint32(iw)), Height: proto.Uint32(uint32(ih)),
		}}
		rec.Thumb = thumb
	case "video":
		var vi videoInfo
		var thumb []byte
		if hasFF {
			vi = probeVideo(path)
			if vi.Codec != "h264" || (ext != ".mp4" && ext != ".m4v") {
				w.send(statusMsg("Conversion de la vidéo en MP4…"))
				tmp, err := transcodeMP4(path, w.mediaDir)
				if err != nil {
					return err
				}
				defer os.Remove(tmp)
				if data, err = os.ReadFile(tmp); err != nil {
					return err
				}
				vi = probeVideo(tmp)
				thumb = videoThumb(tmp)
			} else {
				thumb = videoThumb(path)
			}
			w.send(statusMsg("Envoi de la vidéo…"))
		}
		up, err := w.cli.Upload(ctx, data, whatsmeow.MediaVideo)
		if err != nil {
			return err
		}
		vm := &waE2E.VideoMessage{
			Caption: capPtr, Mimetype: proto.String("video/mp4"),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), JPEGThumbnail: thumb,
		}
		if vi.W > 0 && vi.H > 0 {
			vm.Width, vm.Height = proto.Uint32(uint32(vi.W)), proto.Uint32(uint32(vi.H))
		}
		if vi.Seconds > 0 {
			vm.Seconds = proto.Uint32(uint32(vi.Seconds))
		}
		pm = &waE2E.Message{VideoMessage: vm}
		rec.Thumb = thumb
	case "audio":
		up, err := w.cli.Upload(ctx, data, whatsmeow.MediaAudio)
		if err != nil {
			return err
		}
		secs := uint32(0)
		if hasFF {
			secs = uint32(probeVideo(path).Seconds)
		}
		pm = &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			Mimetype: proto.String(mimeType), Seconds: proto.Uint32(secs),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength),
		}}
		rec.Body = fmt.Sprintf("%d:%02d", secs/60, secs%60)
	default:
		up, err := w.cli.Upload(ctx, data, whatsmeow.MediaDocument)
		if err != nil {
			return err
		}
		name := filepath.Base(path)
		pm = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			Caption: capPtr, Mimetype: proto.String(mimeType),
			FileName: proto.String(name), Title: proto.String(name),
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength),
		}}
		rec.Body = name
		if caption != "" {
			rec.Body += "\n" + caption
		}
	}

	resp, err := w.cli.SendMessage(ctx, jid, pm)
	if err != nil {
		return err
	}
	rec.Kind = kind
	rec.Raw, _ = proto.Marshal(pm)
	w.recordSent(jid, resp, rec)
	return nil
}

func mediaExt(pm *waE2E.Message) string {
	var mt, name string
	switch {
	case pm.GetImageMessage() != nil:
		mt = pm.GetImageMessage().GetMimetype()
	case pm.GetVideoMessage() != nil:
		mt = pm.GetVideoMessage().GetMimetype()
	case pm.GetAudioMessage() != nil:
		mt = pm.GetAudioMessage().GetMimetype()
	case pm.GetStickerMessage() != nil:
		mt = pm.GetStickerMessage().GetMimetype()
	case pm.GetDocumentMessage() != nil:
		mt, name = pm.GetDocumentMessage().GetMimetype(), pm.GetDocumentMessage().GetFileName()
	}
	if e := filepath.Ext(name); e != "" {
		return e
	}
	mt, _, _ = strings.Cut(mt, ";")
	switch mt {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	case "application/pdf":
		return ".pdf"
	}
	if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// Download fetches the media of a message (once) and returns the local file path.
func (w *WA) Download(m Msg) (string, error) {
	if m.Path != "" {
		if _, err := os.Stat(m.Path); err == nil {
			return m.Path, nil
		}
	}
	if len(m.Raw) == 0 {
		return "", errors.New("pas de média dans ce message")
	}
	var pm waE2E.Message
	if err := proto.Unmarshal(m.Raw, &pm); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Minute)
	defer cancel()
	data, err := w.cli.DownloadAny(ctx, &pm)
	if err != nil {
		return "", err
	}
	safe := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, m.ID)
	path := filepath.Join(w.mediaDir, safe+mediaExt(&pm))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	w.st.SetPath(m.Chat, m.ID, path)
	return path, nil
}

// MarkRead sends read receipts for the unread messages of a chat.
func (w *WA) MarkRead(chat string, unread int) {
	jid, err := types.ParseJID(chat)
	if err != nil || unread <= 0 {
		return
	}
	bySender := map[string][]types.MessageID{}
	for _, m := range w.st.LastIncoming(chat, unread) {
		bySender[m.Sender] = append(bySender[m.Sender], m.ID)
	}
	ctx, cancel := context.WithTimeout(w.ctx, 20*time.Second)
	defer cancel()
	for s, ids := range bySender {
		sender := types.EmptyJID
		if jid.Server == types.GroupServer {
			if sender, err = types.ParseJID(s); err != nil {
				continue
			}
		}
		w.cli.MarkRead(ctx, ids, time.Now(), jid, sender)
	}
}
