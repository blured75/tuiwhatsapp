package main

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/atotto/clipboard"
	"github.com/aymanbagabas/go-osc52/v2"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// renderImage draws an image with half-block characters (two pixels per cell, truecolor).
func renderImage(data []byte, maxCols, maxRows int) string {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	sb := src.Bounds()
	if sb.Dx() == 0 || sb.Dy() == 0 || maxCols < 2 || maxRows < 1 {
		return ""
	}
	pw := maxCols
	ph := pw * sb.Dy() / sb.Dx()
	if ph > maxRows*2 {
		ph = maxRows * 2
		pw = ph * sb.Dx() / sb.Dy()
	}
	if ph%2 == 1 {
		ph++
	}
	if pw < 1 || ph < 2 {
		return ""
	}
	dst := image.NewRGBA(image.Rect(0, 0, pw, ph))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)

	var b strings.Builder
	for y := 0; y < ph; y += 2 {
		if y > 0 {
			b.WriteByte('\n')
		}
		for x := 0; x < pw; x++ {
			t := dst.RGBAAt(x, y)
			u := dst.RGBAAt(x, y+1)
			fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀", t.R, t.G, t.B, u.R, u.G, u.B)
		}
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// renderQR draws a QR code with half-blocks, light modules on a dark background inverted
// so that it scans on both light and dark terminal themes.
func renderQR(code string) string {
	q, err := qrcode.New(code, qrcode.Low)
	if err != nil {
		return "QR: " + err.Error()
	}
	bm := q.Bitmap() // true = dark module, includes the quiet zone
	var b strings.Builder
	const on, off = "\x1b[38;2;255;255;255m\x1b[48;2;0;0;0m", "\x1b[0m"
	for y := 0; y < len(bm); y += 2 {
		if y > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(on)
		for x := range bm[y] {
			topLight := !bm[y][x]
			botLight := y+1 >= len(bm) || !bm[y+1][x]
			switch {
			case topLight && botLight:
				b.WriteString("█")
			case topLight:
				b.WriteString("▀")
			case botLight:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString(off)
	}
	return b.String()
}

// makeThumb returns a small JPEG preview plus the original dimensions.
func makeThumb(data []byte) (thumb []byte, w, h int) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0
	}
	w, h = src.Bounds().Dx(), src.Bounds().Dy()
	if w == 0 || h == 0 {
		return nil, 0, 0
	}
	tw, th := 100, 100*h/w
	if h > w {
		th, tw = 100, 100*w/h
	}
	tw, th = max(tw, 1), max(th, 1)
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	if jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 60}) != nil {
		return nil, w, h
	}
	return buf.Bytes(), w, h
}

// toJPEG re-encodes any decodable image (gif, webp, ...) as JPEG.
func toJPEG(data []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	// flatten transparency on white
	dst := image.NewRGBA(src.Bounds())
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Over)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type videoInfo struct {
	Codec   string
	W, H    int
	Seconds int
}

func probeVideo(path string) videoInfo {
	var vi videoInfo
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,width,height:format=duration",
		"-of", "default=nw=1", path).Output()
	if err != nil {
		return vi
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "codec_name":
			vi.Codec = v
		case "width":
			vi.W, _ = strconv.Atoi(v)
		case "height":
			vi.H, _ = strconv.Atoi(v)
		case "duration":
			f, _ := strconv.ParseFloat(v, 64)
			vi.Seconds = int(f + 0.5)
		}
	}
	return vi
}

func videoThumb(path string) []byte {
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-frames:v", "1",
		"-vf", "scale=100:-2", "-f", "image2pipe", "-vcodec", "mjpeg", "-").Output()
	if err != nil || len(out) == 0 {
		return nil
	}
	return out
}

// transcodeMP4 converts a video to H.264/AAC mp4, the format WhatsApp clients play reliably.
func transcodeMP4(path, tmpDir string) (string, error) {
	out := filepath.Join(tmpDir, fmt.Sprintf("send-%d.mp4", os.Getpid()))
	cmd := exec.Command("ffmpeg", "-y", "-v", "error", "-i", path,
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-pix_fmt", "yuv420p",
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		"-c:a", "aac", "-movflags", "+faststart", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg: %v %s", err, strings.TrimSpace(string(b)))
	}
	return out, nil
}

// openExternal opens a file with the desktop's default application, detached from the TUI.
func openExternal(path string) error {
	cmd := exec.Command("xdg-open", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func expandPath(p string) string {
	p = strings.TrimSpace(p)
	// terminals often quote or escape dropped file paths
	if len(p) >= 2 && (p[0] == '\'' && p[len(p)-1] == '\'' || p[0] == '"' && p[len(p)-1] == '"') {
		p = p[1 : len(p)-1]
	}
	p = strings.TrimPrefix(p, "file://")
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	return p
}

// completePath completes a partially typed path; it returns the new value and the candidates.
func completePath(in string) (string, []string) {
	full := expandPath(in)
	if full == "" {
		return in, nil
	}
	matches, _ := filepath.Glob(strings.NewReplacer("[", "\\[", "*", "\\*", "?", "\\?").Replace(full) + "*")
	if len(matches) == 0 {
		return in, nil
	}
	for i, m := range matches {
		if st, err := os.Stat(m); err == nil && st.IsDir() {
			matches[i] = m + "/"
		}
	}
	prefix := matches[0]
	for _, m := range matches[1:] {
		for !strings.HasPrefix(m, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = filepath.Base(strings.TrimSuffix(m, "/"))
		if strings.HasSuffix(m, "/") {
			names[i] += "/"
		}
	}
	if len(prefix) < len(full) {
		prefix = full
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(in, "~") && strings.HasPrefix(prefix, home) {
		prefix = "~" + prefix[len(home):]
	}
	return prefix, names
}

// copyText puts text on the system clipboard, falling back to the terminal's
// OSC 52 escape sequence when no clipboard tool (wl-copy, xclip…) is available.
func copyText(text string) error {
	if err := clipboard.WriteAll(text); err == nil {
		return nil
	}
	_, err := osc52.New(text).WriteTo(os.Stderr)
	return err
}
