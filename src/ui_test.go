package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func testJPEG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 80, 60))
	for y := 0; y < 60; y++ {
		for x := 0; x < 80; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 3), uint8(y * 4), 120, 255})
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, nil)
	return b.Bytes()
}

// Renders the main screen from a seeded store and checks it fits the terminal exactly.
func TestLayoutFitsTerminal(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	wa, err := NewWA(context.Background(), dir, st)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	grp, dm := "123-456@g.us", "33612345678@s.whatsapp.net"
	st.InsertMsgs([]Msg{
		{Chat: grp, ID: "1", Sender: "a@s.whatsapp.net", SenderName: "Alice", TS: now.Add(-26 * time.Hour), Kind: "text", Body: strings.Repeat("Un long message qui doit revenir à la ligne. ", 6)},
		{Chat: grp, ID: "2", Sender: "b@s.whatsapp.net", SenderName: "Bob 🎉", TS: now.Add(-time.Hour), Kind: "image", Body: "Regardez ça", Thumb: testJPEG(), Raw: []byte{1}},
		{Chat: grp, ID: "3", FromMe: true, TS: now, Kind: "video", Thumb: testJPEG(), Raw: []byte{1}},
		{Chat: dm, ID: "4", FromMe: true, TS: now.Add(-time.Minute), Kind: "text", Body: "Salut 👋"},
	})
	st.UpsertChat(grp, "Famille 👨‍👩‍👧 avec un nom de groupe vraiment très long", now, 3)
	st.UpsertChat(dm, "", now.Add(-time.Minute), 0)

	for _, size := range [][2]int{{110, 30}, {60, 14}, {200, 50}} {
		m := newModel(wa, st)
		m.loggedIn = true
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		check := func(stage string) {
			lines := strings.Split(m.View(), "\n")
			if len(lines) != size[1] {
				t.Errorf("%v %s: %d lines, want %d", size, stage, len(lines), size[1])
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > size[0] {
					t.Errorf("%v %s: line %d is %d wide", size, stage, i, w)
				}
			}
		}
		check("list")
		m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		check("chat")
		if m.open == nil || len(m.msgs) != 3 {
			t.Fatalf("chat not opened: %+v", m.open)
		}
		if st.Unread(grp) != 0 {
			t.Errorf("unread not cleared")
		}
		m.Update(tea.KeyMsg{Type: tea.KeyTab})
		m.Update(tea.KeyMsg{Type: tea.KeyUp})
		check("select")
		m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
		check("attach")
		st.UpsertChat(grp, "", now, 3)
	}
}

func TestCompletePath(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "photo-1.jpg"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "photo-2.jpg"), nil, 0o600)
	os.Mkdir(filepath.Join(dir, "videos"), 0o700)
	if got, c := completePath(filepath.Join(dir, "ph")); got != filepath.Join(dir, "photo-") || len(c) != 2 {
		t.Errorf("got %q %v", got, c)
	}
	if got, _ := completePath(filepath.Join(dir, "vi")); got != filepath.Join(dir, "videos")+"/" {
		t.Errorf("got %q", got)
	}
}

func TestQRAndThumb(t *testing.T) {
	if !strings.Contains(renderQR("2@abc,def,ghi"), "█") {
		t.Error("empty QR")
	}
	out := renderImage(testJPEG(), 30, 10)
	if n := len(strings.Split(out, "\n")); n < 5 || n > 10 {
		t.Errorf("thumb has %d rows", n)
	}
	if th, w, h := makeThumb(testJPEG()); len(th) == 0 || w != 80 || h != 60 {
		t.Errorf("makeThumb: %d bytes %dx%d", len(th), w, h)
	}
}
