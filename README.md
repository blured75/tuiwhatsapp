# tuiwhatsapp

A WhatsApp client for the terminal, written in Go with [Bubble Tea](https://github.com/charmbracelet/bubbletea) and [whatsmeow](https://github.com/tulir/whatsmeow).

It links to your account as a companion device (like WhatsApp Web), keeps a local copy of your conversations in SQLite, and renders image thumbnails directly in the terminal.

> The interface is in French.

## Features

- Log in by scanning a QR code shown in the terminal
- Chat list with unread badges, last-message preview and search
- Private and group conversations, with coloured sender names in groups
- Send text messages
- Copy a message's text to the clipboard
- Send photos, videos, audio and documents, with an optional caption and tab completion on the file path
- Inline image and video thumbnails (half-block characters, truecolor)
- Download any received media and open it in the desktop's default application
- Read receipts sent when you open a conversation
- History sync on first connection, stored locally so it is available offline

## Requirements

- Go 1.27.3 (pinned in `mise.toml`; `mise install` sets it up)
- A truecolor terminal, at least 50×10 cells
- `xdg-open`, to open downloaded media
- `wl-copy` (Wayland) or `xclip`/`xsel` (X11), to copy messages; without them the terminal's OSC 52 support is used
- Optional: `ffmpeg` and `ffprobe`, for video thumbnails, durations and conversion to MP4

No C toolchain is needed: SQLite is the pure-Go `modernc.org/sqlite`.

## Build and run

```sh
go build -o build/tuiwhatsapp ./src
./build/tuiwhatsapp
```

Sources live in `src/`; the binary goes to `build/`, which is ignored by git.

On first launch, a QR code is displayed. On your phone, open WhatsApp › Linked devices › Link a device, and scan it. The session is saved, so later launches connect directly.

## Keys

`Ctrl+C` quits from anywhere.

### Chat list

| Key | Action |
| --- | --- |
| `↑` `↓` / `k` `j` | Move the selection |
| `PgUp` `PgDn` | Move by 10 |
| `Home` `End` / `g` `G` | First / last chat |
| `Enter` / `→` / `l` | Open the chat |
| `/` | Search by name (`Enter` opens, `Esc` clears) |
| `Tab` | Go back to the open chat's input |
| `q` | Quit |

### Message input

| Key | Action |
| --- | --- |
| `Enter` | Send the message |
| `Ctrl+O` | Attach a file |
| `Tab` / `↑` | Select messages |
| `PgUp` `PgDn` | Scroll the conversation |
| `Esc` | Back to the chat list |

The mouse wheel also scrolls the conversation.

### Message selection

| Key | Action |
| --- | --- |
| `↑` `↓` / `k` `j` | Move the selection |
| `Home` `End` / `g` `G` | First / last message |
| `y` / `c` | Copy the selected message's text to the clipboard |
| `Enter` / `o` | Download and open the selected media |
| `Esc` / `Tab` / `i` | Back to the input |

### Attaching a file

Press `Ctrl+O`, type a path (`~` is expanded, quoted and `file://` paths dropped from a file manager work), then `Enter`. `Tab` completes the path. Whatever is typed in the message input is sent as the caption.

How the file is sent depends on its type:

| File | Sent as |
| --- | --- |
| JPEG, PNG | Photo |
| Other images (GIF, WebP…) | Photo, converted to JPEG |
| Video | Video; converted to H.264/AAC MP4 with `ffmpeg` if it is not already. Without `ffmpeg`, only `.mp4` is sent as video and other formats go as documents |
| Audio | Audio |
| Anything else | Document |

## Data

Everything is stored under `$XDG_DATA_HOME/tuiwhatsapp` (default `~/.local/share/tuiwhatsapp`), with permissions restricted to your user:

| Path | Content |
| --- | --- |
| `session.db` | WhatsApp device session and encryption keys |
| `messages.db` | Chats and messages |
| `media/` | Downloaded media |

To log out and start over, remove the device from WhatsApp › Linked devices on your phone and delete that directory.

## Tests

```sh
go test ./...
```

## Code layout

All Go sources are in `src/`.

| File | Role |
| --- | --- |
| `main.go` | Entry point, data directory |
| `wa.go` | WhatsApp connection, events, sending and downloading |
| `store.go` | SQLite store for chats and messages |
| `ui.go` | Bubble Tea model, key handling and views |
| `media.go` | Image and QR rendering, thumbnails, `ffmpeg` helpers, path completion |

## Disclaimer

This is an unofficial client, not affiliated with WhatsApp or Meta. Using third-party clients may be against WhatsApp's terms of service; use it at your own risk.
