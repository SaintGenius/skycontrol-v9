package radio

import (
	"fmt"
	"os"
	"strings"
	"time"
)

func (c *NativeClient) handleVoice(raw []byte) {
	c.mu.Lock()
	fn := c.transcribe
	c.mu.Unlock()
	if fn == nil {
		return
	}
	if c.rxBusy.Load() {
		return
	}
	pkt, err := decodeVoicePacket(raw)
	if err != nil || pkt == nil {
		return
	}
	if len(pkt.Audio) < 3 {
		return
	}
	guid := strings.TrimRight(string(pkt.OriginGUID), "\x00")
	if guid == "" || guid == c.guid {
		return
	}
	freq := Frequency{Hz: 305_000_000, Modulation: "AM"}
	if len(pkt.Freqs) > 0 {
		freq.Hz = pkt.Freqs[0].Hz
		if pkt.Freqs[0].Mod == 1 {
			freq.Modulation = "FM"
		} else {
			freq.Modulation = "AM"
		}
	}

	c.rxMu.Lock()
	defer c.rxMu.Unlock()
	if c.rxGUID != "" && c.rxGUID != guid {
		// Someone else won the radio first; ignore overlap.
		if time.Since(c.rxLast) < 400*time.Millisecond {
			return
		}
		c.finishRXLocked()
	}
	if pkt.PacketID > 0 && pkt.PacketID <= c.rxPktID && c.rxGUID == guid {
		return
	}
	if c.rxGUID == "" {
		c.rxGUID = guid
		c.rxName = c.peerName(guid)
		c.rxFreq = freq
		c.rxFrames = nil
		fmt.Printf("  SRS RX %.3f %s from %s\n", freq.Hz/1_000_000, freq.Modulation, c.rxName)
	}
	c.rxPktID = pkt.PacketID
	c.rxLast = time.Now()
	cp := make([]byte, len(pkt.Audio))
	copy(cp, pkt.Audio)
	c.rxFrames = append(c.rxFrames, cp)
	if len(c.rxFrames) > 400 { // ~16s
		c.finishRXLocked()
	}
}

func (c *NativeClient) flushRX(force bool) {
	c.rxMu.Lock()
	defer c.rxMu.Unlock()
	if c.rxGUID == "" {
		return
	}
	if !force && time.Since(c.rxLast) < 450*time.Millisecond {
		return
	}
	c.finishRXLocked()
}

func (c *NativeClient) finishRXLocked() {
	frames := c.rxFrames
	name := c.rxName
	freq := c.rxFreq
	c.rxGUID = ""
	c.rxName = ""
	c.rxFrames = nil
	c.rxPktID = 0
	if len(frames) < 6 { // < ~240ms
		return
	}
	c.mu.Lock()
	fn := c.transcribe
	c.mu.Unlock()
	if fn == nil {
		fmt.Println("  SRS RX: heard a call (no whisper hooked)")
		return
	}
	go c.transcribeRX(frames, name, freq, fn)
}

func (c *NativeClient) transcribeRX(frames [][]byte, name string, freq Frequency, fn func(string) (string, error)) {
	wav, err := framesToWAV(frames)
	if err != nil {
		fmt.Printf("  SRS RX decode failed: %v\n", err)
		c.log.Warn("SRS RX decode failed", "error", err)
		return
	}
	defer os.Remove(wav)
	fmt.Println("  SRS whisper...")
	text, err := fn(wav)
	if err != nil {
		fmt.Printf("  SRS whisper failed: %v\n", err)
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		fmt.Println("  SRS RX: nothing usable")
		return
	}
	fmt.Printf("  YOU SAID (SRS): %s\n", text)
	call := ReceivedCall{
		Pilot:      name,
		Frequency:  freq,
		Transcript: text,
		ReceivedAt: time.Now(),
	}
	select {
	case c.rxChan <- call:
	default:
		c.log.Warn("RX queue full, dropping call")
	}
}
