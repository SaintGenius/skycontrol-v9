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

type Frequency struct {
	Hz         float64
	Modulation string
}

func (f Frequency) String() string {
	mhz := f.Hz / 1_000_000
	return fmt.Sprintf("%.3f %s", mhz, f.Modulation)
}

type Transmission struct {
	Text       string
	Spoken     string
	AudioPCM   []float32
	OpusFrames [][]byte
	AudioFile  string
	KeepFile   bool
	Frequency  Frequency
	ExtraFreqs []Frequency
	Coalition  int
	Callsign   string
	Priority   int
	CreatedAt  time.Time
}

type ReceivedCall struct {
	Pilot      string
	GUID       string // SRS client GUID — sticky lock to a Tacview jet
	Frequency  Frequency
	AudioPCM   []float32
	Transcript string
	ReceivedAt time.Time
}

type Client interface {
	Run(ctx context.Context) error
	Transmit(tx Transmission)
	Received() <-chan ReceivedCall
	Frequencies() []Frequency
	SetFrequencies(freqs []Frequency)
}

type Config struct {
	Address     string
	ClientName  string
	EAMPassword string
	Coalition   int
	Frequencies []Frequency
	Log         *slog.Logger
}

type StubClient struct {
	cfg     Config
	log     *slog.Logger
	mu      sync.RWMutex
	freqs   []Frequency
	txQueue chan Transmission
	rxChan  chan ReceivedCall
}

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

func (c *StubClient) Received() <-chan ReceivedCall { return c.rxChan }

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

func MustParseFrequency(s string) Frequency {
	f, err := ParseFrequency(s)
	if err != nil {
		panic(err)
	}
	return f
}