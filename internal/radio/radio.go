package radio

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Frequency represents a radio frequency in Hz with modulation.
type Frequency struct {
	Hz         float64 // e.g. 251_000_000 for 251.0 MHz
	Modulation string  // "AM" or "FM"
}

// String returns a human-readable frequency, e.g. "251.000 AM".
func (f Frequency) String() string {
	mhz := f.Hz / 1_000_000
	return fmt.Sprintf("%.3f %s", mhz, f.Modulation)
}

// Transmission represents audio or text to send over the radio.
type Transmission struct {
	Text      string    // spoken line (used by Windows TTS)
	Spoken    string    // original ATC line with commas (Piper file, optional)
	AudioPCM  []float32 // optional pre-rendered PCM
	OpusFrames [][]byte // optional cached SRS voice frames (ATIS); skips Piper
	AudioFile  string   // optional WAV path to play as-is (ATIS loop); not deleted
	KeepFile   bool     // if true, send() will not delete piper temp wav
	Frequency Frequency
	ExtraFreqs []Frequency // nearest-field tower/ground, stacked with config freqs
	Coalition int // 0=spectator, 1=red, 2=blue
	Callsign  string
	Priority  int
	CreatedAt time.Time
}

// ReceivedCall is a voice transmission received from a player.
type ReceivedCall struct {
	Pilot      string
	Frequency  Frequency
	AudioPCM   []float32 // raw audio if available
	Transcript string    // filled later by STT
	ReceivedAt time.Time
}

// Client is the interface for talking to SRS.
// This lets us swap implementations (native client vs ExternalAudio helper).
type Client interface {
	// Run starts the client and blocks until context is cancelled.
	Run(ctx context.Context) error

	// Transmit queues a transmission to be spoken on the radio.
	Transmit(tx Transmission)

	// Received returns a channel of incoming voice calls (after STT later).
	Received() <-chan ReceivedCall

	// Frequencies returns the frequencies this client is currently on.
	Frequencies() []Frequency

	// SetFrequencies updates the frequencies the client monitors/transmits on.
	SetFrequencies(freqs []Frequency)
}

// Config holds SRS connection settings.
type Config struct {
	Address     string // "host:5002"
	ClientName  string // appears in SRS client list
	EAMPassword string // External AWACS Mode password
	Coalition   int    // 0=spectator, 1=red, 2=blue
	Frequencies []Frequency
	Log         *slog.Logger
}

// StubClient is a placeholder implementation that logs actions.
// It allows the rest of the system to be developed and tested
// without a live SRS server. Later we replace it with a full client.
type StubClient struct {
	cfg     Config
	log     *slog.Logger
	mu      sync.RWMutex
	freqs   []Frequency
	txQueue chan Transmission
	rxChan  chan ReceivedCall
}

// NewStubClient creates a non-functional but interface-compliant client.
func NewStubClient(cfg Config) *StubClient {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &StubClient{
		cfg:     cfg,
		log:     log,
		freqs:   append([]Frequency(nil), cfg.Frequencies...),
		txQueue: make(chan Transmission, 32),
		rxChan:  make(chan ReceivedCall, 32),
	}
}

func (c *StubClient) Run(ctx context.Context) error {
	c.log.Info("SRS stub client started (no real radio connection yet)",
		"address", c.cfg.Address,
		"name", c.cfg.ClientName,
		"frequencies", len(c.freqs),
	)

	for {
		select {
		case <-ctx.Done():
			c.log.Info("SRS stub client stopped")
			return ctx.Err()
		case tx := <-c.txQueue:
			c.log.Info("SRS TX (stub)",
				"callsign", tx.Callsign,
				"freq", tx.Frequency.String(),
				"text", tx.Text,
			)
			// Real client would encode to Opus and send via UDP here.
		}
	}
}

func (c *StubClient) Transmit(tx Transmission) {
	tx.CreatedAt = time.Now()
	select {
	case c.txQueue <- tx:
	default:
		c.log.Warn("TX queue full, dropping transmission")
	}
}

func (c *StubClient) Received() <-chan ReceivedCall {
	return c.rxChan
}

func (c *StubClient) Frequencies() []Frequency {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Frequency, len(c.freqs))
	copy(out, c.freqs)
	return out
}

func (c *StubClient) SetFrequencies(freqs []Frequency) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.freqs = append([]Frequency(nil), freqs...)
	c.log.Info("SRS frequencies updated", "count", len(freqs))
}

// ParseFrequency converts a string like "251.0AM", "251.0 AM", or "251.0" into a Frequency.
func ParseFrequency(s string) (Frequency, error) {
	s = strings.TrimSpace(s)
	mod := "AM"
	upper := strings.ToUpper(s)
	if strings.HasSuffix(upper, "AM") {
		mod = "AM"
		s = strings.TrimSpace(s[:len(s)-2])
	} else if strings.HasSuffix(upper, "FM") {
		mod = "FM"
		s = strings.TrimSpace(s[:len(s)-2])
	}

	mhz, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return Frequency{}, fmt.Errorf("invalid frequency %q: %w", s, err)
	}
	return Frequency{Hz: mhz * 1_000_000, Modulation: mod}, nil
}

// MustParseFrequency is like ParseFrequency but panics on error (for static config).
func MustParseFrequency(s string) Frequency {
	f, err := ParseFrequency(s)
	if err != nil {
		panic(err)
	}
	return f
}
