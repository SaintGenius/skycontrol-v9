package radio

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// NativeClient stays connected to SRS and streams voice packets itself.
// It does not launch DCS-SR-ExternalAudio.exe.
type NativeClient struct {
	cfg        Config
	log        *slog.Logger
	speakerTTS func(string) (string, error)
	transcribe func(string) (string, error)

	mu        sync.Mutex
	tcp       net.Conn
	udp       *net.UDPConn
	guid      string
	name      string
	coalition int
	freq      Frequency
	packetID  uint64
	busy      func(bool)
	txQueue   chan Transmission
	rxChan    chan ReceivedCall
	freqs     []Frequency
	peers     map[string]string
	listenOnly bool
	connected  atomic.Bool
	pos        srsPosition

	rxMu     sync.Mutex
	rxGUID   string
	rxName   string
	rxFreq   Frequency
	rxFrames [][]byte
	rxLast   time.Time
	rxPktID  uint64
	rxBusy   atomic.Bool
}

func NewNativeClient(cfg Config, textToWav func(string) (string, error)) *NativeClient {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	coalition := cfg.Coalition
	if coalition == 0 {
		coalition = 2
	}
	name := cfg.ClientName
	if name == "" {
		name = "Sky Control"
	}
	f := Frequency{Hz: 305_000_000, Modulation: "AM"}
	if len(cfg.Frequencies) > 0 {
		f = cfg.Frequencies[0]
	}
	return &NativeClient{
		cfg:        cfg,
		log:        log,
		speakerTTS: textToWav,
		guid:       newSRSGUID(),
		name:       name,
		coalition:  coalition,
		freq:       f,
		packetID:   1,
		txQueue:    make(chan Transmission, 16),
		rxChan:     make(chan ReceivedCall, 8),
		freqs:      append([]Frequency(nil), cfg.Frequencies...),
		peers:      make(map[string]string),
	}
}

// NewListenClient is a silent SRS radio: hear players, do not talk.
func NewListenClient(cfg Config) *NativeClient {
	if cfg.ClientName == "" {
		cfg.ClientName = "Sky Control"
	}
	c := NewNativeClient(cfg, nil)
	c.listenOnly = true
	return c
}

func (c *NativeClient) SetPosition(lat, lon, altFt float64) {
	c.mu.Lock()
	c.pos = srsPosition{Latitude: lat, Longitude: lon, Altitude: altFt * 0.3048}
	c.mu.Unlock()
}

func (c *NativeClient) SetTranscriber(fn func(wavPath string) (string, error)) {
	c.mu.Lock()
	c.transcribe = fn
	c.mu.Unlock()
}

func (c *NativeClient) Connected() bool {
	return c != nil && c.connected.Load()
}

func (c *NativeClient) Run(ctx context.Context) error {
	kind := "native"
	if c.listenOnly {
		kind = "listen"
	}
	c.log.Info("SRS native client starting", "mode", kind, "address", c.cfg.Address, "guid", c.guid, "name", c.name)
	if c.listenOnly {
		fmt.Printf("  SRS listen: %s @ %s\n", c.name, c.cfg.Address)
	} else {
		fmt.Println("  ATC radio: SRS native (stays connected)")
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.connect(ctx); err != nil {
			c.connected.Store(false)
			c.log.Warn("SRS native connect failed, retrying", "error", err)
			fmt.Printf("  SRS %s: connect failed (%v) — retry in %s\n", kind, err, backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		c.connected.Store(true)
		fmt.Printf("  SRS %s: connected (%d freqs)\n", kind, len(c.Frequencies()))
		err := c.serve(ctx)
		c.connected.Store(false)
		c.closeConn()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.log.Warn("SRS native disconnected, reconnecting", "error", err)
		fmt.Printf("  SRS %s: dropped — reconnecting\n", kind)
	}
}

func (c *NativeClient) connect(ctx context.Context) error {
	d := net.Dialer{Timeout: 5 * time.Second}
	tcp, err := d.DialContext(ctx, "tcp", c.cfg.Address)
	if err != nil {
		return fmt.Errorf("tcp: %w", err)
	}
	udpAddr, err := net.ResolveUDPAddr("udp", c.cfg.Address)
	if err != nil {
		_ = tcp.Close()
		return err
	}
	udp, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		_ = tcp.Close()
		return fmt.Errorf("udp: %w", err)
	}
	c.mu.Lock()
	c.tcp = tcp
	c.udp = udp
	c.mu.Unlock()

	if err := c.sendJSON(c.syncMessage()); err != nil {
		c.closeConn()
		return fmt.Errorf("sync: %w", err)
	}
	if pw := strings.TrimSpace(c.cfg.EAMPassword); pw != "" {
		msg := c.syncMessage()
		msg.Type = 7 // ExternalAWACSModePassword
		msg.ExternalAWACSModePassword = pw
		_ = c.sendJSON(msg)
	}
	_ = c.sendJSON(c.radioUpdateMessage())
	c.udpPing()
	return nil
}

func (c *NativeClient) serve(ctx context.Context) error {
	errCh := make(chan error, 4)
	go func() { errCh <- c.readTCP(ctx) }()
	go func() { errCh <- c.readUDP(ctx) }()
	go func() { errCh <- c.pingLoop(ctx) }()
	go func() { errCh <- c.rxFlushLoop(ctx) }()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if err != nil && ctx.Err() == nil {
				return err
			}
		case tx := <-c.txQueue:
			if c.listenOnly {
				continue
			}
			if err := c.sendTX(tx); err != nil {
				c.log.Error("native TX failed", "error", err)
				fmt.Printf("  SRS native TX failed: %v\n", err)
			}
		}
	}
}

func (c *NativeClient) sendTX(tx Transmission) error {
	if c.listenOnly {
		return nil
	}
	if c.busy != nil && tx.Priority >= 0 {
		c.busy(true)
		defer c.busy(false)
	}
	c.rxBusy.Store(true)
	defer c.rxBusy.Store(false)

	freq := pickNativeFreq(tx)
	name := tx.Callsign
	if name == "" {
		name = c.cfg.ClientName
	}
	if name == "" {
		name = "Sky Control"
	}

	c.mu.Lock()
	needUpdate := c.name != name || absHz(c.freq.Hz-freq.Hz) > 500
	c.name = name
	c.freq = freq
	c.mu.Unlock()
	if needUpdate {
		if err := c.sendJSON(c.radioUpdateMessage()); err != nil {
			return fmt.Errorf("radio update: %w", err)
		}
		time.Sleep(80 * time.Millisecond)
	}

	src := strings.TrimSpace(tx.Spoken)
	if src == "" {
		src = tx.Text
	}
	src = expandForPiper(src)
	if src == "" && len(tx.OpusFrames) == 0 {
		return fmt.Errorf("no text")
	}
	if c.speakerTTS == nil && len(tx.OpusFrames) == 0 {
		return fmt.Errorf("no piper")
	}
	var frames [][]byte
	if len(tx.OpusFrames) > 0 {
		frames = tx.OpusFrames
	} else {
		wav, err := c.speakerTTS(src)
		if err != nil || wav == "" {
			if err == nil {
				err = fmt.Errorf("empty wav")
			}
			return err
		}
		frames, err = wavToOpusFrames(wav)
		if err != nil {
			return err
		}
	}
	dur := time.Duration(len(frames)) * 40 * time.Millisecond
	if tx.Priority >= 0 {
		fmt.Printf("  SRS native TX %.3f %s as %s — %.1fs (%d frames)\n",
			freq.Hz/1_000_000, orAM(freq.Modulation), name, dur.Seconds(), len(frames))
	}

	c.mu.Lock()
	udp := c.udp
	guid := c.guid
	startID := c.packetID
	c.packetID += uint64(len(frames))
	c.mu.Unlock()
	if udp == nil {
		return fmt.Errorf("no udp")
	}

	mod := byte(0)
	if strings.EqualFold(freq.Modulation, "FM") {
		mod = 1
	}
	freqs := []srsFreq{{Hz: freq.Hz, Mod: mod}}
	start := time.Now()
	for i, opus := range frames {
		pkt := encodeVoicePacket(opus, freqs, 100000002, startID+uint64(i), []byte(guid))
		delay := time.Until(start.Add(time.Duration(i)*40*time.Millisecond - 20*time.Millisecond))
		if delay > 0 {
			time.Sleep(delay)
		}
		if _, err := udp.Write(pkt); err != nil {
			return err
		}
	}
	if tx.Priority >= 0 {
		fmt.Printf("  SRS native done %.1fs\n", time.Since(start).Seconds())
	}
	return nil
}

func pickNativeFreq(tx Transmission) Frequency {
	uhf := func(list []Frequency) Frequency {
		var vhf Frequency
		for _, f := range list {
			if f.Hz >= 200_000_000 {
				return f
			}
			if vhf.Hz == 0 && f.Hz >= 1_000_000 {
				vhf = f
			}
		}
		return vhf
	}
	if tx.Frequency.Hz >= 1_000_000 {
		return tx.Frequency
	}
	if f := uhf(tx.ExtraFreqs); f.Hz > 0 {
		return f
	}
	return Frequency{Hz: 305_000_000, Modulation: "AM"}
}

func orAM(m string) string {
	if m == "" {
		return "AM"
	}
	return m
}

func absHz(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func (c *NativeClient) pingLoop(ctx context.Context) error {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	c.udpPing()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			_ = c.sendJSON(c.typedMessage(1)) // Ping
			c.udpPing()
		}
	}
}

func (c *NativeClient) udpPing() {
	c.mu.Lock()
	udp, guid := c.udp, c.guid
	c.mu.Unlock()
	if udp != nil {
		_, _ = udp.Write([]byte(guid))
	}
}

func (c *NativeClient) readTCP(ctx context.Context) error {
	c.mu.Lock()
	tcp := c.tcp
	c.mu.Unlock()
	if tcp == nil {
		return fmt.Errorf("no tcp")
	}
	r := bufio.NewReader(tcp)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = tcp.SetReadDeadline(time.Now().Add(30 * time.Second))
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		var msg srsMessage
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		c.noteClients(msg)
		if msg.Type == 6 { // VersionMismatch
			c.log.Warn("SRS version mismatch", "server", msg.Version)
		}
	}
}

func (c *NativeClient) noteClients(msg srsMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.peers == nil {
		c.peers = make(map[string]string)
	}
	add := func(info srsClientInfo) {
		if info.GUID == "" {
			return
		}
		if info.GUID == c.guid {
			return
		}
		n := strings.TrimSpace(info.Name)
		if n == "" {
			n = "Pilot"
		}
		c.peers[info.GUID] = n
	}
	add(msg.Client)
	for _, p := range msg.Clients {
		add(p)
	}
}

func (c *NativeClient) peerName(guid string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := c.peers[guid]; n != "" {
		return n
	}
	return "Pilot"
}

func (c *NativeClient) readUDP(ctx context.Context) error {
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.mu.Lock()
		udp := c.udp
		c.mu.Unlock()
		if udp == nil {
			return fmt.Errorf("no udp")
		}
		_ = udp.SetReadDeadline(time.Now().Add(20 * time.Second))
		n, err := udp.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		if n == 22 {
			continue // GUID ping
		}
		c.handleVoice(buf[:n])
	}
}

func (c *NativeClient) rxFlushLoop(ctx context.Context) error {
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			c.flushRX(false)
		}
	}
}

func (c *NativeClient) closeConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tcp != nil {
		_ = c.tcp.Close()
		c.tcp = nil
	}
	if c.udp != nil {
		_ = c.udp.Close()
		c.udp = nil
	}
}

func (c *NativeClient) Transmit(tx Transmission) {
	if c.listenOnly {
		return
	}
	tx.CreatedAt = time.Now()
	select {
	case c.txQueue <- tx:
	default:
		c.log.Warn("TX queue full, dropping transmission")
	}
}

func (c *NativeClient) Received() <-chan ReceivedCall { return c.rxChan }

func (c *NativeClient) Frequencies() []Frequency {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Frequency, len(c.freqs))
	copy(out, c.freqs)
	return out
}

func (c *NativeClient) SetFrequencies(freqs []Frequency) {
	c.mu.Lock()
	same := len(freqs) == len(c.freqs)
	if same {
		for i := range freqs {
			if absHz(freqs[i].Hz-c.freqs[i].Hz) > 500 {
				same = false
				break
			}
		}
	}
	c.freqs = append([]Frequency(nil), freqs...)
	if len(c.freqs) > 0 && c.freq.Hz < 1_000_000 {
		c.freq = c.freqs[0]
	}
	need := !same && c.tcp != nil
	c.mu.Unlock()
	if need {
		_ = c.sendJSON(c.radioUpdateMessage())
	}
}

func (c *NativeClient) SetBusy(fn func(bool)) {
	if c != nil {
		c.busy = fn
	}
}

type srsMessage struct {
	Version                   string            `json:"Version"`
	Client                    srsClientInfo     `json:"Client"`
	Clients                   []srsClientInfo   `json:"Clients,omitempty"`
	ServerSettings            map[string]string `json:"ServerSettings,omitempty"`
	ExternalAWACSModePassword string            `json:"ExternalAWACSModePassword,omitempty"`
	Type                      int               `json:"MsgType"`
}

type srsClientInfo struct {
	GUID           string       `json:"ClientGuid"`
	Name           string       `json:"Name"`
	Seat           int          `json:"Seat"`
	Coalition      int          `json:"Coalition"`
	AllowRecording bool         `json:"AllowRecord"`
	RadioInfo      srsRadioInfo `json:"RadioInfo"`
	Position       srsPosition  `json:"LatLngPosition"`
}

type srsRadioInfo struct {
	Radios  []srsRadio `json:"radios"`
	Unit    string     `json:"unit"`
	UnitID  uint64     `json:"unitId"`
	IFF     srsIFF     `json:"iff"`
	Ambient srsAmbient `json:"ambient"`
}

type srsRadio struct {
	Frequency        float64 `json:"freq"`
	Modulation       byte    `json:"modulation"`
	IsEncrypted      bool    `json:"enc"`
	EncryptionKey    byte    `json:"encKey"`
	GuardFrequency   float64 `json:"secFreq"`
	ShouldRetransmit bool    `json:"retransmit"`
}

type srsIFF struct {
	Control int  `json:"control"`
	Status  int  `json:"status"`
	Mode1   int  `json:"mode1"`
	Mode2   int  `json:"mode2"`
	Mode3   int  `json:"mode3"`
	Mode4   bool `json:"mode4"`
	Mic     int  `json:"mic"`
}

type srsAmbient struct {
	Volume float64 `json:"vol"`
	Type   string  `json:"abType"`
}

type srsPosition struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lng"`
	Altitude  float64 `json:"alt"`
}

func (c *NativeClient) clientInfo() srsClientInfo {
	c.mu.Lock()
	name, freq, coal, guid, freqs, pos := c.name, c.freq, c.coalition, c.guid, append([]Frequency(nil), c.freqs...), c.pos
	c.mu.Unlock()
	radios := make([]srsRadio, 0, 11)
	seen := map[int64]bool{}
	add := func(f Frequency) {
		if f.Hz < 1_000_000 {
			return
		}
		key := int64(f.Hz / 1000)
		if seen[key] {
			return
		}
		seen[key] = true
		mod := byte(0)
		if strings.EqualFold(f.Modulation, "FM") {
			mod = 1
		}
		radios = append(radios, srsRadio{
			Frequency:      f.Hz,
			Modulation:     mod,
			GuardFrequency: -1,
		})
	}
	add(freq)
	for _, f := range freqs {
		add(f)
	}
	if len(radios) == 0 {
		add(Frequency{Hz: 305_000_000, Modulation: "AM"})
	}
	if len(radios) > 11 {
		radios = radios[:11]
	}
	return srsClientInfo{
		GUID:           guid,
		Name:           name,
		Seat:           0,
		Coalition:      coal,
		AllowRecording: true,
		RadioInfo: srsRadioInfo{
			Radios: radios,
			Unit:   name,
			UnitID: 100000002,
			IFF: srsIFF{
				Control: 2, Status: 0,
				Mode1: -1, Mode2: -1, Mode3: -1, Mic: -1,
			},
			Ambient: srsAmbient{Volume: 1},
		},
		Position: pos,
	}
}

func (c *NativeClient) typedMessage(t int) srsMessage {
	return srsMessage{
		Version: "2.4.0.0",
		Type:    t,
		Client:  c.clientInfo(),
	}
}

func (c *NativeClient) syncMessage() srsMessage        { return c.typedMessage(2) } // Sync
func (c *NativeClient) radioUpdateMessage() srsMessage { return c.typedMessage(3) } // RadioUpdate

func (c *NativeClient) sendJSON(msg srsMessage) error {
	c.mu.Lock()
	tcp := c.tcp
	c.mu.Unlock()
	if tcp == nil {
		return fmt.Errorf("no tcp")
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_ = tcp.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = tcp.Write(b)
	return err
}

func newSRSGUID() string {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	b := make([]byte, 22)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			b[i] = alphabet[i%len(alphabet)]
			continue
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}
