package main

import (
	"database/sql"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Msg is one chat message as stored locally.
type Msg struct {
	Chat       string
	ID         string
	Sender     string
	SenderName string
	FromMe     bool
	TS         time.Time
	Kind       string // text, image, video, audio, doc, sticker, other
	Body       string
	Thumb      []byte // JPEG thumbnail, if any
	Raw        []byte // serialized protobuf, kept for media so it can be downloaded later
	Path       string // local file once downloaded (or the original file for sent media)
}

func (m Msg) IsMedia() bool {
	switch m.Kind {
	case "image", "video", "audio", "doc", "sticker":
		return true
	}
	return false
}

// Chat is one conversation in the list.
type Chat struct {
	JID      string
	Name     string
	LastTS   time.Time
	Unread   int
	LastKind string
	LastBody string
	LastMine bool
}

func (c Chat) IsGroup() bool { return strings.HasSuffix(c.JID, "@g.us") }

func (c Chat) Title() string {
	if c.Name != "" {
		return c.Name
	}
	user, _, _ := strings.Cut(c.JID, "@")
	if c.IsGroup() {
		return "Groupe " + user
	}
	return "+" + user
}

type Store struct{ db *sql.DB }

func sqliteDSN(path string) string {
	return "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
}

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS chats(
	jid TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	last_ts INTEGER NOT NULL DEFAULT 0,
	unread INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages(
	chat TEXT NOT NULL,
	id TEXT NOT NULL,
	sender TEXT NOT NULL DEFAULT '',
	sender_name TEXT NOT NULL DEFAULT '',
	from_me INTEGER NOT NULL DEFAULT 0,
	ts INTEGER NOT NULL,
	kind TEXT NOT NULL,
	body TEXT NOT NULL DEFAULT '',
	thumb BLOB,
	raw BLOB,
	path TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(chat, id)
);
CREATE INDEX IF NOT EXISTS messages_chat_ts ON messages(chat, ts);
`)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

// InsertMsgs stores messages (ignoring ones already known) and returns how many were new.
func (s *Store) InsertMsgs(msgs []Msg) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO messages(chat,id,sender,sender_name,from_me,ts,kind,body,thumb,raw,path)
		VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(chat,id) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	n := 0
	for _, m := range msgs {
		res, err := stmt.Exec(m.Chat, m.ID, m.Sender, m.SenderName, m.FromMe, m.TS.Unix(), m.Kind, m.Body, m.Thumb, m.Raw, m.Path)
		if err != nil {
			return n, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n++
		}
	}
	return n, tx.Commit()
}

// UpsertChat creates the chat or bumps its timestamp/unread counter. An empty name keeps the old one.
func (s *Store) UpsertChat(jid, name string, ts time.Time, unreadDelta int) error {
	_, err := s.db.Exec(`INSERT INTO chats(jid,name,last_ts,unread) VALUES(?,?,?,?)
		ON CONFLICT(jid) DO UPDATE SET
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE name END,
			last_ts = MAX(last_ts, excluded.last_ts),
			unread = unread + excluded.unread`,
		jid, name, ts.Unix(), unreadDelta)
	return err
}

func (s *Store) SetChatName(jid, name string) {
	if name != "" {
		s.db.Exec(`UPDATE chats SET name=? WHERE jid=?`, name, jid)
	}
}

func (s *Store) ChatName(jid string) string {
	var name string
	s.db.QueryRow(`SELECT name FROM chats WHERE jid=?`, jid).Scan(&name)
	return name
}

func (s *Store) ClearUnread(jid string) {
	s.db.Exec(`UPDATE chats SET unread=0 WHERE jid=?`, jid)
}

func (s *Store) ChatJIDs() []string {
	rows, err := s.db.Query(`SELECT jid FROM chats`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var j string
		if rows.Scan(&j) == nil {
			out = append(out, j)
		}
	}
	return out
}

func (s *Store) Chats() ([]Chat, error) {
	rows, err := s.db.Query(`
SELECT c.jid, c.name, c.last_ts, c.unread,
	COALESCE(m.kind,''), COALESCE(m.body,''), COALESCE(m.from_me,0)
FROM chats c
LEFT JOIN messages m ON m.rowid = (
	SELECT rowid FROM messages WHERE chat = c.jid ORDER BY ts DESC, rowid DESC LIMIT 1)
ORDER BY c.last_ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		var ts int64
		if err := rows.Scan(&c.JID, &c.Name, &ts, &c.Unread, &c.LastKind, &c.LastBody, &c.LastMine); err != nil {
			return nil, err
		}
		c.LastTS = time.Unix(ts, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Messages returns the latest `limit` messages of a chat, oldest first.
func (s *Store) Messages(chat string, limit int) ([]Msg, error) {
	rows, err := s.db.Query(`SELECT chat,id,sender,sender_name,from_me,ts,kind,body,thumb,raw,path FROM (
		SELECT *, rowid AS r FROM messages WHERE chat=? ORDER BY ts DESC, rowid DESC LIMIT ?)
		ORDER BY ts ASC, r ASC`, chat, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Msg
	for rows.Next() {
		var m Msg
		var ts int64
		if err := rows.Scan(&m.Chat, &m.ID, &m.Sender, &m.SenderName, &m.FromMe, &ts, &m.Kind, &m.Body, &m.Thumb, &m.Raw, &m.Path); err != nil {
			return nil, err
		}
		m.TS = time.Unix(ts, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// LastIncoming returns the n most recent messages received in a chat (id + sender only).
func (s *Store) LastIncoming(chat string, n int) []Msg {
	rows, err := s.db.Query(`SELECT id, sender, ts FROM messages WHERE chat=? AND from_me=0 ORDER BY ts DESC, rowid DESC LIMIT ?`, chat, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Msg
	for rows.Next() {
		var m Msg
		var ts int64
		if rows.Scan(&m.ID, &m.Sender, &ts) == nil {
			m.TS = time.Unix(ts, 0)
			out = append(out, m)
		}
	}
	return out
}

func (s *Store) Unread(chat string) int {
	var n int
	s.db.QueryRow(`SELECT unread FROM chats WHERE jid=?`, chat).Scan(&n)
	return n
}

func (s *Store) UpdateBody(chat, id, kind, body string) {
	s.db.Exec(`UPDATE messages SET kind=?, body=?, thumb=NULL, raw=NULL WHERE chat=? AND id=?`, kind, body, chat, id)
}

func (s *Store) EditBody(chat, id, body string) {
	s.db.Exec(`UPDATE messages SET body=? WHERE chat=? AND id=?`, body, chat, id)
}

func (s *Store) SetPath(chat, id, path string) {
	s.db.Exec(`UPDATE messages SET path=? WHERE chat=? AND id=?`, path, chat, id)
}
