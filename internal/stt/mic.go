package stt

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type Mic struct {
	log       *slog.Logger
	mu        sync.Mutex
	atcMute   time.Time
	hangUntil time.Time
	pttDown   bool
	pttAt     time.Time
	lastHeard time.Time
	cmd       *exec.Cmd
	pttName   string
	pttVK     int
	pttPad    uint16
	always    bool
	flagPath  string
	ctx       context.Context
	out       chan<- string
	paused    bool
	pttOnce   sync.Once
	whisper   *Whisper
	recMu     sync.Mutex
	recording bool
	recStart  int
	srsPrimary bool
}

func NewMic(log *slog.Logger, pttKey string) *Mic {
	if log == nil {
		log = slog.Default()
	}
	m := &Mic{log: log, pttName: strings.ToUpper(strings.TrimSpace(pttKey))}
	m.pttVK, m.pttPad, m.always = parsePTT(m.pttName)
	m.flagPath = os.TempDir() + `\skycontrol-ptt.on`
	return m
}

func (m *Mic) Deaf(d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	until := time.Now().Add(d)
	if until.After(m.atcMute) {
		m.atcMute = until
	}
	m.mu.Unlock()
}

func (m *Mic) Hear() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.atcMute = time.Time{}
	m.mu.Unlock()
}

func (m *Mic) live() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().Before(m.atcMute) {
		return false
	}
	if m.always {
		return true
	}
	if m.pttDown {
		return true
	}
	return time.Now().Before(m.hangUntil)
}

func (m *Mic) setPTT(down bool) {
	m.mu.Lock()
	m.pttDown = down
	if down {
		m.hangUntil = time.Time{}
		m.pttAt = time.Now()
		_ = os.WriteFile(m.flagPath, []byte("1"), 0644)
		m.mu.Unlock()
		go pttClick(true)
		return
	}
	start := m.pttAt
	whisper := m.whisper != nil
	m.hangUntil = time.Now().Add(3 * time.Second)
	m.mu.Unlock()
	go pttClick(false)
	go func(path string) {
		time.Sleep(3 * time.Second)
		_ = os.Remove(path)
	}(m.flagPath)
	if whisper {
		return
	}
	go func(start time.Time) {
		time.Sleep(2500 * time.Millisecond)
		m.mu.Lock()
		heard := m.lastHeard.After(start)
		m.mu.Unlock()
		if !heard {
			fmt.Println("  (no words heard — hold Radio 1 the whole time you talk)")
		}
	}(start)
}

func (m *Mic) PauseCapture() {
	if m == nil {
		return
	}
	pauseWaveRecord()
}

func (m *Mic) ResumeCapture() {
	if m == nil || m.whisper == nil {
		return
	}
	if err := startWaveRecord(); err != nil {
		fmt.Printf("  mic reopen failed: %v\n", err)
	}
}

func (m *Mic) Pause() {
	if m == nil {
		return
	}
	m.Deaf(30 * time.Second)
}

func (m *Mic) Resume() {
	if m == nil {
		return
	}
	m.Hear()
}

func (m *Mic) SetSRSPrimary(on bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	was := m.srsPrimary
	m.srsPrimary = on
	m.mu.Unlock()
	if on && !was {
		fmt.Println("  Xbox mic off — talk with SRS PTT (COM1)")
	}
}

func (m *Mic) usingSRS() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.srsPrimary
}

func (m *Mic) SetWhisper(apiKey, baseURL string) {
	if m == nil {
		return
	}
	w := NewWhisper(apiKey, baseURL, m.log)
	if w.Enabled() {
		m.whisper = w
	}
}

func (m *Mic) Start(ctx context.Context, out chan<- string) error {
	m.mu.Lock()
	m.ctx = ctx
	m.out = out
	m.mu.Unlock()
	m.pttOnce.Do(func() { go m.pttLoop(ctx) })
	m.log.Info("mic ready", "ptt", m.pttName, "always", m.always, "whisper", m.whisper != nil)
	if m.whisper != nil {
		fmt.Println("  whisper ON — hold Radio 1, then speak")
		if err := startWaveRecord(); err != nil {
			fmt.Printf("  whisper rec failed: %v\n", err)
			return err
		}
		fmt.Println("  mic open (leave this window visible)")
		playbackReady()
		go func() {
			time.Sleep(400 * time.Millisecond)
			pttClick(true)
			time.Sleep(140 * time.Millisecond)
			pttClick(false)
		}()
		return nil
	}
	return m.startEngine()
}

func (m *Mic) startEngine() error {
	m.mu.Lock()
	if m.paused || m.ctx == nil || m.out == nil {
		m.mu.Unlock()
		return nil
	}
	if m.cmd != nil {
		m.mu.Unlock()
		return nil
	}
	ctx := m.ctx
	out := m.out
	m.mu.Unlock()

	script := os.TempDir() + `\skycontrol-listen.ps1`
	if err := os.WriteFile(script, []byte(listenPS1), 0644); err != nil {
		return fmt.Errorf("write listen script: %w", err)
	}
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-ExecutionPolicy", "Bypass",
		"-WindowStyle", "Hidden", "-File", script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start speech engine: %w", err)
	}
	m.mu.Lock()
	m.cmd = cmd
	m.mu.Unlock()

	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			if line == "MIC_READY" {
				fmt.Println("  speech engine ready")
				continue
			}
			if strings.HasPrefix(line, "MIC_ERROR") {
				fmt.Printf("  mic error: %s\n", line)
				continue
			}
			if !m.live() {
				continue
			}
			m.mu.Lock()
			m.lastHeard = time.Now()
			m.mu.Unlock()
			select {
			case out <- line:
			case <-ctx.Done():
				return
			}
		}
		_ = cmd.Wait()
		m.mu.Lock()
		if m.cmd == cmd {
			m.cmd = nil
		}
		m.mu.Unlock()
	}()
	return nil
}

func (m *Mic) pttLoop(ctx context.Context) {
	if m.always {
		_ = os.WriteFile(m.flagPath, []byte("1"), 0644)
		return
	}
	if m.pttVK == 0 && m.pttPad == 0 {
		fmt.Println("  PTT off — type only")
		return
	}
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	held := false
	var lastEdge time.Time
	for {
		select {
		case <-ctx.Done():
			_ = os.Remove(m.flagPath)
			return
		case <-ticker.C:
			down := false
			if m.pttPad != 0 {
				down = padDown(m.pttPad)
			}
			if !down && m.pttVK != 0 {
				down = vkDown(m.pttVK)
			}
			if time.Since(lastEdge) < 180*time.Millisecond {
				continue
			}
			if down && !held {
				held = true
				lastEdge = time.Now()
				m.setPTT(true)
				fmt.Println("  PTT down")
				if m.whisper != nil && !m.usingSRS() {
					go m.beginRecord()
				}
			} else if !down && held {
				held = false
				lastEdge = time.Now()
				m.setPTT(false)
				fmt.Println("  PTT up")
				if m.whisper != nil && !m.usingSRS() {
					go m.finishRecord()
				}
			}
		}
	}
}

func (m *Mic) beginRecord() {
	if !m.live() {
		fmt.Println("  (wait — ATC is transmitting)")
		return
	}
	fmt.Println("  recording...")
	if err := startWaveRecord(); err != nil {
		fmt.Printf("  whisper rec failed: %v\n", err)
		m.recMu.Lock()
		m.recording = false
		m.recMu.Unlock()
		return
	}
	m.recMu.Lock()
	m.recStart = recordMark()
	m.recording = true
	m.recMu.Unlock()
}

func (m *Mic) finishRecord() {
	path := filepath.Join(os.TempDir(), "skycontrol-ptt.wav")
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		m.recMu.Lock()
		on := m.recording
		m.recMu.Unlock()
		if on || time.Now().After(deadline) {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}

	m.recMu.Lock()
	on := m.recording
	start := m.recStart
	m.recording = false
	m.recMu.Unlock()
	if !on {
		fmt.Println("  (no recording — mic may be locked by SRS)")
		return
	}

	err := clipFrom(start, path)
	if err != nil {
		fmt.Printf("  whisper save failed: %v\n", err)
		return
	}
	if m.whisper == nil {
		return
	}
	fmt.Println("  whisper listening...")
	text, err := m.whisper.Transcribe(path)
	_ = os.Remove(path)
	if err != nil {
		fmt.Printf("  whisper failed: %v\n", err)
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		fmt.Println("  (nothing usable — talk while holding PTT, closer to the mic)")
		return
	}
	fmt.Printf("  YOU SAID: %s\n", text)
	m.mu.Lock()
	m.lastHeard = time.Now()
	out := m.out
	ctx := m.ctx
	m.mu.Unlock()
	if out == nil {
		return
	}
	select {
	case out <- text:
	case <-ctx.Done():
	}
}

func pttClick(down bool) {
	if down {
		pttTone(880, 90)
	} else {
		pttTone(520, 80)
	}
}

func pttTone(hz, ms uintptr) {
	if err := playTone(int(hz), int(ms)); err != nil {
		fmt.Printf("  beep failed: %v\n", err)
	}
}

func parsePTT(name string) (vk int, pad uint16, always bool) {
	switch name {
	case "", "ALWAYS", "ON":
		return 0, 0, true
	case "OFF", "NONE", "NO":
		return 0, 0, false
	case "LSHIFT":
		return 0xA0, 0, false
	case "RSHIFT":
		return 0xA1, 0, false
	case "LCTRL", "LCONTROL":
		return 0xA2, 0, false
	case "RCTRL", "RCONTROL":
		return 0xA3, 0, false
	case "LALT", "LMENU":
		return 0xA4, 0, false
	case "RALT", "RMENU":
		return 0xA5, 0, false
	case "CAPS", "CAPSLOCK":
		return 0x14, 0, false
	case "SPACE":
		return 0x20, 0, false
	case "TAB":
		return 0x09, 0, false
	case "MOUSE4", "XBUTTON1":
		return 0x05, 0, false
	case "MOUSE5", "XBUTTON2":
		return 0x06, 0, false
	case "PAD1", "JOY1", "XBOX-A", "CONTROLLER1":
		return 0, 0x1000, false
	case "PAD2", "JOY2", "XBOX-B":
		return 0, 0x2000, false
	case "PAD3", "JOY3", "XBOX-X":
		return 0, 0x4000, false
	case "PAD4", "JOY4", "XBOX-Y":
		return 0, 0x8000, false
	case "PAD5", "JOY5", "LB":
		return 0, 0x0100, false
	case "PAD6", "JOY6", "RB":
		return 0, 0x0200, false
	}
	if strings.HasPrefix(name, "F") && len(name) <= 3 {
		n, err := strconv.Atoi(name[1:])
		if err == nil && n >= 1 && n <= 24 {
			return 0x70 + n - 1, 0, false
		}
	}
	if n, err := strconv.Atoi(name); err == nil {
		return n, 0, false
	}
	return 0xA3, 0, false
}

func writeToneWAV(path string, hz, ms int) error {
	sr := 16000
	n := sr * ms / 1000
	if n < 100 {
		n = 100
	}
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(math.Sin(2*math.Pi*float64(hz)*float64(i)/float64(sr)) * 18000)
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(v))
	}
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

func vkDown(vk int) bool {
	if vk == 0 {
		return false
	}
	r, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return r&0x8000 != 0
}

type xiState struct {
	packet  uint32
	buttons uint16
	lt, rt  uint8
	lx, ly  int16
	rx, ry  int16
}

func padDown(mask uint16) bool {
	if procXInputGetState == nil {
		return false
	}
	for i := uintptr(0); i < 4; i++ {
		var st xiState
		r, _, _ := procXInputGetState.Call(i, uintptr(unsafe.Pointer(&st)))
		if r == 0 && st.buttons&mask != 0 {
			return true
		}
	}
	return false
}

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	winmm                = syscall.NewLazyDLL("winmm.dll")
	procGetAsyncKeyState = user32.NewProc("GetAsyncKeyState")
	procPlaySound        = winmm.NewProc("PlaySoundW")
	procXInputGetState   *syscall.LazyProc
)

func init() {
	for _, dll := range []string{"xinput1_4.dll", "xinput1_3.dll", "xinput9_1_0.dll"} {
		x := syscall.NewLazyDLL(dll)
		p := x.NewProc("XInputGetState")
		if err := p.Find(); err == nil {
			procXInputGetState = p
			return
		}
	}
}

const listenPS1 = `
$ErrorActionPreference = 'SilentlyContinue'
Add-Type -AssemblyName System.Speech
$culture = [System.Globalization.CultureInfo]::GetCultureInfo('en-US')
$eng = New-Object System.Speech.Recognition.SpeechRecognitionEngine $culture
try {
  $eng.SetInputToDefaultAudioDevice()
} catch {
  Write-Output ("MIC_ERROR " + $_.Exception.Message)
  exit 1
}
$eng.InitialSilenceTimeout = [TimeSpan]::FromSeconds(2)
$eng.EndSilenceTimeout = [TimeSpan]::FromMilliseconds(600)
$phrases = @(
  'need a taxi','request taxi','requesting taxi','ready to taxi','taxi','taxi to',
  'ready for departure','ready for takeoff','request takeoff','requesting takeoff',
  'request landing','request inbound','inbound','in bound','calling inbound',
  'on final','final','full stop',
  'touch and go','the option',
  'request parking','request startup','requesting startup','request start',
  'start my aircraft','I would like to start',
  'radio check','go around','going around','hold short',
  'say again','say again last transmission','repeat last','repeat',
  'what is the active runway','active runway','what is my heading','whats my heading',
  'current heading','say heading','how far','how far from the field','where am I',
  'what is my altitude'
)
$choices = New-Object System.Speech.Recognition.Choices
foreach ($p in $phrases) { [void]$choices.Add($p) }
$gb = New-Object System.Speech.Recognition.GrammarBuilder
$gb.Culture = $culture
$gb.Append($choices)
$eng.LoadGrammar((New-Object System.Speech.Recognition.Grammar $gb))
$eng.LoadGrammar((New-Object System.Speech.Recognition.DictationGrammar))
Write-Output 'MIC_READY'
[Console]::Out.Flush()
while ($true) {
  $r = $eng.Recognize([TimeSpan]::FromSeconds(8))
  if ($null -ne $r) {
    $t = $r.Text.Trim()
    if ($t -ne '') {
      Write-Output $t
      [Console]::Out.Flush()
    }
  }
}
`
