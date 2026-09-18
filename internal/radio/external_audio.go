package radio

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ExternalAudioClient struct {
	cfg        Config
	log        *slog.Logger
	exePath    string
	mu         sync.Mutex
	freqs      []Frequency
	txQueue    chan Transmission
	rxChan     chan ReceivedCall
	speakerTTS func(text string) (wavPath string, err error)
	busy       func(bool)
	lastCmd    *exec.Cmd
	noKill     bool
}

type ExternalAudioConfig struct {
	Config
	ExePath   string
	TextToWav func(text string) (wavPath string, err error)
	NoKill    bool // ATIS: do not taskkill other ExternalAudio processes
}

func NewExternalAudioClient(cfg ExternalAudioConfig) (*ExternalAudioClient, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	exe := cfg.ExePath
	if exe == "" {
		exe = findExternalAudioExe()
	}
	if exe == "" {
		return nil, fmt.Errorf("DCS-SR-ExternalAudio.exe not found; set srs-external-audio in config.yaml")
	}
	if _, err := os.Stat(exe); err != nil {
		return nil, fmt.Errorf("DCS-SR-ExternalAudio.exe not found at %s", exe)
	}
	coalition := cfg.Coalition
	if coalition == 0 {
		coalition = 2
	}
	cfg.Coalition = coalition
	return &ExternalAudioClient{
		cfg:        cfg.Config,
		log:        log,
		exePath:    exe,
		freqs:      append([]Frequency(nil), cfg.Frequencies...),
		txQueue:    make(chan Transmission, 16),
		rxChan:     make(chan ReceivedCall, 8),
		speakerTTS: cfg.TextToWav,
		noKill:     cfg.NoKill,
	}, nil
}

func findExternalAudioExe() string {
	candidates := []string{
		`C:\Program Files\DCS-SimpleRadio-Standalone\ExternalAudio\DCS-SR-ExternalAudio.exe`,
		filepath.Join(os.Getenv("PROGRAMFILES"), "DCS-SimpleRadio-Standalone", "ExternalAudio", "DCS-SR-ExternalAudio.exe"),
		filepath.Join(os.Getenv("PROGRAMFILES"), "DCS-SimpleRadio-Standalone", "DCS-SR-ExternalAudio.exe"),
		"DCS-SR-ExternalAudio.exe",
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func (c *ExternalAudioClient) Run(ctx context.Context) error {
	if !c.noKill {
		KillOrphanExternalAudio()
	}
	c.log.Info("SRS ExternalAudio client started",
		"exe", c.exePath, "address", c.cfg.Address,
		"frequencies", len(c.freqs), "coalition", c.cfg.Coalition, "nokill", c.noKill)
	for {
		select {
		case <-ctx.Done():
			if c.lastCmd != nil && c.lastCmd.Process != nil {
				_ = c.lastCmd.Process.Kill()
			}
			if !c.noKill {
				KillOrphanExternalAudio()
			}
			return ctx.Err()
		case tx := <-c.txQueue:
			if err := c.send(tx); err != nil {
				c.log.Error("ExternalAudio transmit failed", "error", err, "text", tx.Text)
				if tx.Priority >= 0 {
					fmt.Printf("  SRS TX failed: %v\n", err)
				}
			}
		}
	}
}

func KillOrphanExternalAudio() {
	if runtime.GOOS != "windows" {
		return
	}
	cmd := exec.Command("taskkill", "/IM", "DCS-SR-ExternalAudio.exe", "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

func (c *ExternalAudioClient) freqsFor(tx Transmission) []Frequency {
	seen := map[int64]bool{}
	var freqs []Frequency
	add := func(f Frequency) {
		if f.Hz < 1_000_000 {
			return
		}
		key := int64(f.Hz / 1000)
		if seen[key] {
			return
		}
		seen[key] = true
		if f.Modulation == "" {
			f.Modulation = "AM"
		}
		freqs = append(freqs, f)
	}
	if len(tx.ExtraFreqs) > 0 {
		add(tx.ExtraFreqs[0])
	} else {
		add(tx.Frequency)
		for _, f := range c.freqs {
			add(f)
		}
	}
	if len(freqs) == 0 {
		add(Frequency{Hz: 305_000_000, Modulation: "AM"})
	}
	if len(freqs) > 5 {
		freqs = freqs[:5]
	}
	return freqs
}

func (c *ExternalAudioClient) send(tx Transmission) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	freqs := c.freqsFor(tx)
	coalition := tx.Coalition
	if coalition == 0 {
		coalition = c.cfg.Coalition
	}
	if coalition == 0 {
		coalition = 2
	}
	name := tx.Callsign
	if name == "" {
		name = c.cfg.ClientName
	}
	if name == "" {
		name = "Sky Control"
	}
	if tx.Text == "" && tx.AudioFile == "" {
		return fmt.Errorf("no text to transmit")
	}
	if c.busy != nil && tx.Priority >= 0 {
		c.busy(true)
		defer c.busy(false)
	}

	host := hostFromAddress(c.cfg.Address)
	port := portFromAddress(c.cfg.Address)
	_ = host

	var file, kind string
	var hold time.Duration
	var err error
	keep := tx.KeepFile || tx.AudioFile != ""
	if tx.AudioFile != "" {
		file = tx.AudioFile
		kind = "wav"
		hold = wavHold(file)
		if hold < time.Second {
			hold = speakDuration(tx.Text)
		}
	} else {
		file, kind, hold, err = c.piperFile(tx)
		if err != nil || file == "" {
			need := speakDuration(tx.Text)
			if tx.Priority >= 0 {
				fmt.Printf("  SRS TX (windows TTS) as %s (%.1fs)\n", name, need.Seconds())
			}
			for i, f := range freqs {
				fs := fmt.Sprintf("%.3f", f.Hz/1_000_000)
				if tx.Priority >= 0 {
					fmt.Printf("  SRS TX %s %s as %s [%d/%d]\n", fs, f.Modulation, name, i+1, len(freqs))
				}
				args := []string{
					"-f", fs, "-m", f.Modulation,
					"-c", strconv.Itoa(coalition),
					"-p", port, "-n", name, "-v", "1.0",
					"-s", "-1",
					"-t", flatSpeak(tx.Text),
					"-g", "male", "-l", "en-US",
				}
				_ = c.runExe(args, need+2*time.Second, true)
			}
			return nil
		}
	}
	if !keep {
		defer os.Remove(file)
	}
	if tx.Priority >= 0 {
		if err := copyFile(file, "last-atc.wav"); err == nil {
			fmt.Println("  saved last-atc.wav")
		}
	}

	for i, f := range freqs {
		fs := fmt.Sprintf("%.3f", f.Hz/1_000_000)
		if tx.Priority >= 0 {
			fmt.Printf("  SRS TX %s %s as %s (%.1fs %s) [%d/%d]\n",
				fs, f.Modulation, name, hold.Seconds(), kind, i+1, len(freqs))
		}
		args := []string{
			"-f", fs, "-m", f.Modulation,
			"-c", strconv.Itoa(coalition),
			"-p", port, "-n", name, "-v", "1.0",
			"-i", file,
		}
		if err := c.runExe(args, hold+2*time.Second, true); err != nil {
			if tx.Priority >= 0 {
				fmt.Printf("  SRS TX failed %s: %v\n", fs, err)
			}
		}
	}
	return nil
}

func (c *ExternalAudioClient) runExe(args []string, hold time.Duration, hidden bool) error {
	c.log.Info("SRS TX via ExternalAudio", "args", strings.Join(args, " "))
	if c.lastCmd != nil && c.lastCmd.Process != nil {
		_ = c.lastCmd.Process.Kill()
		c.lastCmd = nil
	}
	cmd := exec.Command(c.exePath, args...)
	cmd.Dir = filepath.Dir(c.exePath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: hidden}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.lastCmd = cmd
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	start := time.Now()
	var err error
	select {
	case err = <-done:
		elapsed := time.Since(start)
		if !c.noKill {
			if hold > time.Second && elapsed < hold*7/10 {
				fmt.Printf("  SRS dropped early (ran %.1fs of %.1fs file)\n", elapsed.Seconds(), hold.Seconds())
			} else {
				fmt.Printf("  SRS done %.1fs (file %.1fs)\n", elapsed.Seconds(), hold.Seconds())
			}
		}
	case <-time.After(hold + 15*time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		err = <-done
		if !c.noKill {
			fmt.Println("  SRS TX timed out, killed")
		}
	}
	if c.lastCmd == cmd {
		c.lastCmd = nil
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	time.Sleep(250 * time.Millisecond)
	return err
}

func (c *ExternalAudioClient) piperFile(tx Transmission) (path, kind string, hold time.Duration, err error) {
	if c.speakerTTS == nil {
		return "", "", 0, fmt.Errorf("no piper")
	}
	src := strings.TrimSpace(tx.Spoken)
	if src == "" {
		src = tx.Text
	}
	src = expandForPiper(src)
	if src == "" {
		return "", "", 0, fmt.Errorf("no text")
	}

	wav, err := c.speakerTTS(src)
	if err != nil || wav == "" {
		if err == nil {
			err = fmt.Errorf("empty wav")
		}
		return "", "", 0, err
	}

	w, err := readPCMWav(wav)
	if err != nil {
		_ = os.Remove(wav)
		return "", "", 0, err
	}
	fillSilence(w.pcm, w.sr)
	w.pcm = padTail(w.pcm, w.sr, 2.0)
	filled := strings.TrimSuffix(wav, filepath.Ext(wav)) + "-filled.wav"
	if err := writePCMWav(filled, w.pcm, w.sr); err != nil {
		_ = os.Remove(wav)
		return "", "", 0, err
	}
	_ = os.Remove(wav)
	samples := len(w.pcm) / 2
	if w.sr < 1 {
		w.sr = 22050
	}
	hold = time.Duration(samples) * time.Second / time.Duration(w.sr)
	if !c.noKill {
		fmt.Printf("  piper wav %.1fs (incl. 2s tail)\n", hold.Seconds())
	}
	return filled, "wav", hold, nil
}

var digitWord = map[rune]string{
	'0': "zero", '1': "one", '2': "two", '3': "three", '4': "four",
	'5': "five", '6': "six", '7': "seven", '8': "eight", '9': "niner",
}

func expandForPiper(s string) string {
	s = strings.ReplaceAll(s, "You're", "you are")
	s = strings.ReplaceAll(s, "you're", "you are")
	s = strings.ReplaceAll(s, "-", " ")
	var b strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if w, ok := digitWord[r]; ok {
			b.WriteByte(' ')
			b.WriteString(w)
			if i+1 < len(runes) && runes[i+1] == '.' && i+2 < len(runes) && digitWord[runes[i+2]] != "" {
				b.WriteString(" point ")
				i++
			}
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	s = strings.Join(strings.Fields(b.String()), " ")
	s = commaNumberWords(s)
	return spaceSentences(s)
}

func spaceSentences(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, r := range runes {
		b.WriteRune(r)
		if r == '.' && i+1 < len(runes) && runes[i+1] != ' ' && runes[i+1] != '.' {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func commaNumberWords(s string) string {
	num := map[string]bool{
		"zero": true, "one": true, "two": true, "three": true, "four": true,
		"five": true, "six": true, "seven": true, "eight": true, "niner": true,
		"nine": true, "lima": true, "left": true, "right": true, "point": true,
	}
	words := strings.Fields(s)
	isNum := func(w string) bool {
		return num[strings.ToLower(strings.Trim(w, ",."))]
	}
	var out []string
	for i, w := range words {
		out = append(out, w)
		if i+1 < len(words) && isNum(w) && isNum(words[i+1]) {
			continue
		}
	}
	return strings.Join(out, " ")
}

func fillSilence(pcm []byte, sr int) {
	_ = sr
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int16(binary.LittleEndian.Uint16(pcm[i : i+2]))
		if v == 0 {
			n := int16((i/2)%3 - 1)
			if n == 0 {
				n = 1
			}
			binary.LittleEndian.PutUint16(pcm[i:i+2], uint16(n))
		}
	}
}

func padTail(pcm []byte, sr int, sec float64) []byte {
	if sr < 1 {
		sr = 22050
	}
	n := int(float64(sr)*sec) * 2
	if n < 2 {
		return pcm
	}
	tail := make([]byte, n)
	for i := 0; i+1 < len(tail); i += 2 {
		v := int16((i/2)%3 - 1)
		if v == 0 {
			v = 1
		}
		binary.LittleEndian.PutUint16(tail[i:i+2], uint16(v))
	}
	return append(pcm, tail...)
}

type pcmWav struct {
	sr  int
	pcm []byte
}

func readPCMWav(path string) (*pcmWav, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a wav file")
	}
	sr := 22050
	var data []byte
	i := 12
	for i+8 <= len(b) {
		id := string(b[i : i+4])
		sz := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		body := i + 8
		if body+sz > len(b) {
			sz = len(b) - body
		}
		switch id {
		case "fmt ":
			if sz >= 16 {
				bits := int(binary.LittleEndian.Uint16(b[body+14 : body+16]))
				sr = int(binary.LittleEndian.Uint32(b[body+4 : body+8]))
				if bits != 16 {
					return nil, fmt.Errorf("need 16-bit pcm")
				}
			}
		case "data":
			data = b[body : body+sz]
		}
		i = body + sz
		if sz&1 == 1 {
			i++
		}
	}
	if len(data) < 100 {
		return nil, fmt.Errorf("empty wav data")
	}
	if sr < 8000 {
		sr = 22050
	}
	return &pcmWav{sr: sr, pcm: data}, nil
}

func writePCMWav(path string, pcm []byte, sr int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dataLen := uint32(len(pcm))
	var hdr [44]byte
	copy(hdr[0:], []byte("RIFF"))
	binary.LittleEndian.PutUint32(hdr[4:], 36+dataLen)
	copy(hdr[8:], []byte("WAVEfmt "))
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1)
	binary.LittleEndian.PutUint16(hdr[22:], 1)
	binary.LittleEndian.PutUint32(hdr[24:], uint32(sr))
	binary.LittleEndian.PutUint32(hdr[28:], uint32(sr*2))
	binary.LittleEndian.PutUint16(hdr[32:], 2)
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], []byte("data"))
	binary.LittleEndian.PutUint32(hdr[40:], dataLen)
	if _, err := f.Write(hdr[:]); err != nil {
		return err
	}
	_, err = f.Write(pcm)
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func flatSpeak(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ',', '.', ';', ':', '!', '?', '-', '\'', '"', '(', ')', '…':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func (c *ExternalAudioClient) SetBusy(fn func(bool)) {
	if c != nil {
		c.busy = fn
	}
}

func speakDuration(text string) time.Duration {
	n := len(strings.Fields(text))
	if n < 4 {
		n = 4
	}
	ms := 1500 + int(float64(n)/2.0*1000)
	return time.Duration(ms) * time.Millisecond
}

func (c *ExternalAudioClient) Transmit(tx Transmission) {
	tx.CreatedAt = time.Now()
	select {
	case c.txQueue <- tx:
	default:
		c.log.Warn("TX queue full, dropping transmission")
	}
}

func (c *ExternalAudioClient) Received() <-chan ReceivedCall { return c.rxChan }

func (c *ExternalAudioClient) Frequencies() []Frequency {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Frequency, len(c.freqs))
	copy(out, c.freqs)
	return out
}

func (c *ExternalAudioClient) SetFrequencies(freqs []Frequency) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.freqs = append([]Frequency(nil), freqs...)
}

func portFromAddress(addr string) string {
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return addr[idx+1:]
	}
	return "5002"
}

func hostFromAddress(addr string) string {
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return addr[:idx]
	}
	return addr
}

func wavHold(path string) time.Duration {
	w, err := readPCMWav(path)
	if err != nil || w == nil || w.sr < 1 {
		return 0
	}
	samples := len(w.pcm) / 2
	return time.Duration(samples) * time.Second / time.Duration(w.sr)
}

// PrepareLoopWav copies a Piper WAV into dst with silence filled and a short tail.
func PrepareLoopWav(src, dst string, tailSec float64) (time.Duration, error) {
	w, err := readPCMWav(src)
	if err != nil {
		return 0, err
	}
	fillSilence(w.pcm, w.sr)
	if tailSec > 0 {
		w.pcm = padTail(w.pcm, w.sr, tailSec)
	}
	if err := writePCMWav(dst, w.pcm, w.sr); err != nil {
		return 0, err
	}
	if w.sr < 1 {
		w.sr = 22050
	}
	samples := len(w.pcm) / 2
	return time.Duration(samples) * time.Second / time.Duration(w.sr), nil
}

func NewClientFromConfig(cfg Config, exePath string, textToWav func(string) (string, error)) Client {
	_ = exePath
	return NewNativeClient(cfg, textToWav)
}
