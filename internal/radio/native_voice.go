package radio

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type srsFreq struct {
	Hz  float64
	Mod byte
}

type voicePacket struct {
	Audio      []byte
	Freqs      []srsFreq
	UnitID     uint32
	PacketID   uint64
	Hops       byte
	RelayGUID  []byte
	OriginGUID []byte
}

const (
	srsHeaderLen = 6
	srsFixedLen  = 58
	srsFreqLen   = 10
	srsGUIDLen   = 22
)

func encodeVoicePacket(opus []byte, freqs []srsFreq, unitID uint32, packetID uint64, guid []byte) []byte {
	if len(guid) > srsGUIDLen {
		guid = guid[:srsGUIDLen]
	}
	if len(guid) < srsGUIDLen {
		g := make([]byte, srsGUIDLen)
		copy(g, guid)
		guid = g
	}
	if len(opus) > 0xFFFF {
		opus = opus[:0xFFFF]
	}
	audioLen := uint16(len(opus))
	freqLen := uint16(len(freqs) * srsFreqLen)
	total := uint16(srsHeaderLen) + audioLen + freqLen + srsFixedLen
	b := make([]byte, total)
	binary.LittleEndian.PutUint16(b[0:2], total)
	binary.LittleEndian.PutUint16(b[2:4], audioLen)
	binary.LittleEndian.PutUint16(b[4:6], freqLen)
	copy(b[srsHeaderLen:], opus)
	off := srsHeaderLen + int(audioLen)
	for i, f := range freqs {
		o := off + i*srsFreqLen
		binary.LittleEndian.PutUint64(b[o:o+8], math.Float64bits(f.Hz))
		b[o+8] = f.Mod
		b[o+9] = 0
	}
	// 1-byte pad then unitID, packetID, hops, relay GUID, origin GUID
	fixed := int(total) - srsFixedLen + 1
	binary.LittleEndian.PutUint32(b[fixed:fixed+4], unitID)
	binary.LittleEndian.PutUint64(b[fixed+4:fixed+12], packetID)
	b[fixed+12] = 0
	copy(b[fixed+13:fixed+13+srsGUIDLen], guid)
	copy(b[fixed+13+srsGUIDLen:], guid)
	return b
}

func decodeVoicePacket(b []byte) (*voicePacket, error) {
	if len(b) < srsHeaderLen+srsFixedLen-1 {
		return nil, fmt.Errorf("short packet")
	}
	total := binary.LittleEndian.Uint16(b[0:2])
	if int(total) > len(b) {
		total = uint16(len(b))
	}
	if total < uint16(srsHeaderLen+srsFixedLen-1) {
		return nil, fmt.Errorf("bad length")
	}
	originPtr := int(total) - srsGUIDLen
	relayPtr := originPtr - srsGUIDLen
	hopsPtr := relayPtr - 1
	pktPtr := hopsPtr - 8
	unitPtr := pktPtr - 4
	if unitPtr < srsHeaderLen {
		return nil, fmt.Errorf("truncated")
	}
	audioLen := int(binary.LittleEndian.Uint16(b[2:4]))
	freqLen := int(binary.LittleEndian.Uint16(b[4:6]))
	if srsHeaderLen+audioLen+freqLen > int(total) {
		return nil, fmt.Errorf("segment overflow")
	}
	audio := make([]byte, audioLen)
	copy(audio, b[srsHeaderLen:srsHeaderLen+audioLen])
	var freqs []srsFreq
	fs := b[srsHeaderLen+audioLen : srsHeaderLen+audioLen+freqLen]
	for i := 0; i+srsFreqLen <= len(fs); i += srsFreqLen {
		freqs = append(freqs, srsFreq{
			Hz:  math.Float64frombits(binary.LittleEndian.Uint64(fs[i : i+8])),
			Mod: fs[i+8],
		})
	}
	orig := make([]byte, srsGUIDLen)
	copy(orig, b[originPtr:originPtr+srsGUIDLen])
	relay := make([]byte, srsGUIDLen)
	copy(relay, b[relayPtr:relayPtr+srsGUIDLen])
	return &voicePacket{
		Audio:      audio,
		Freqs:      freqs,
		UnitID:     binary.LittleEndian.Uint32(b[unitPtr : unitPtr+4]),
		PacketID:   binary.LittleEndian.Uint64(b[pktPtr : pktPtr+8]),
		Hops:       b[hopsPtr],
		RelayGUID:  relay,
		OriginGUID: orig,
	}, nil
}

func WavToOpusFrames(wav string) ([][]byte, error) {
	return wavToOpusFrames(wav)
}

func wavToOpusFrames(wav string) ([][]byte, error) {
	ff := findFFmpegBin()
	if ff == "" {
		return nil, fmt.Errorf("ffmpeg not found (needed for native SRS voice)")
	}
	ogg := strings.TrimSuffix(wav, filepath.Ext(wav)) + ".ogg"
	cmd := exec.Command(ff, "-y", "-i", wav,
		"-ac", "1", "-ar", "16000",
		"-c:a", "libopus", "-application", "voip",
		"-frame_duration", "40", "-vbr", "off", "-b:a", "24000",
		"-f", "ogg", ogg)
	hideSRSExec(cmd)
	out, err := cmd.CombinedOutput()
	_ = os.Remove(wav)
	if err != nil {
		_ = os.Remove(ogg)
		msg := strings.TrimSpace(string(out))
		if len(msg) > 180 {
			msg = msg[len(msg)-180:]
		}
		return nil, fmt.Errorf("ffmpeg opus: %v %s", err, msg)
	}
	raw, err := os.ReadFile(ogg)
	_ = os.Remove(ogg)
	if err != nil {
		return nil, err
	}
	pkts := extractOpusPackets(raw)
	if len(pkts) < 2 {
		return nil, fmt.Errorf("no opus frames in ogg (%d bytes)", len(raw))
	}
	return pkts, nil
}

func framesToWAV(frames [][]byte) (string, error) {
	if len(frames) == 0 {
		return "", fmt.Errorf("no frames")
	}
	ff := findFFmpegBin()
	if ff == "" {
		return "", fmt.Errorf("ffmpeg not found")
	}
	ogg := buildOggOpus(frames, 16000)
	in := filepath.Join(os.TempDir(), "skycontrol-rx.ogg")
	out := filepath.Join(os.TempDir(), "skycontrol-rx.wav")
	if err := os.WriteFile(in, ogg, 0644); err != nil {
		return "", err
	}
	defer os.Remove(in)
	cmd := exec.Command(ff, "-y", "-i", in, "-ac", "1", "-ar", "16000", "-c:a", "pcm_s16le", out)
	hideSRSExec(cmd)
	b, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 180 {
			msg = msg[len(msg)-180:]
		}
		return "", fmt.Errorf("ffmpeg wav: %v %s", err, msg)
	}
	return out, nil
}

func extractOpusPackets(ogg []byte) [][]byte {
	var out [][]byte
	headers := 0
	i := 0
	for i+27 <= len(ogg) {
		if ogg[i] != 'O' || ogg[i+1] != 'g' || ogg[i+2] != 'g' || ogg[i+3] != 'S' {
			i++
			continue
		}
		nseg := int(ogg[i+26])
		if i+27+nseg > len(ogg) {
			break
		}
		table := ogg[i+27 : i+27+nseg]
		p := i + 27 + nseg
		var pkt []byte
		for _, s := range table {
			end := p + int(s)
			if end > len(ogg) {
				return out
			}
			pkt = append(pkt, ogg[p:end]...)
			p = end
			if s < 255 {
				if headers < 2 {
					headers++
				} else if len(pkt) > 0 {
					cp := make([]byte, len(pkt))
					copy(cp, pkt)
					out = append(out, cp)
				}
				pkt = nil
			}
		}
		i = p
	}
	return out
}

func findFFmpegBin() string {
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	for _, c := range []string{
		`C:\ffmpeg\bin\ffmpeg.exe`,
		`C:\Program Files\ffmpeg\bin\ffmpeg.exe`,
		`C:\Program Files (x86)\ffmpeg\bin\ffmpeg.exe`,
	} {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func buildOggOpus(frames [][]byte, sampleRate uint32) []byte {
	head := make([]byte, 19)
	copy(head[0:], []byte("OpusHead"))
	head[8] = 1
	head[9] = 1
	binary.LittleEndian.PutUint16(head[10:12], 312)
	binary.LittleEndian.PutUint32(head[12:16], sampleRate)
	vendor := []byte("SkyControl")
	tags := make([]byte, 8+4+len(vendor)+4)
	copy(tags[0:], []byte("OpusTags"))
	binary.LittleEndian.PutUint32(tags[8:12], uint32(len(vendor)))
	copy(tags[12:], vendor)
	serial := uint32(0x534b4331) // "SKC1"
	var out []byte
	out = append(out, oggPage(0x02, 0, serial, 0, head)...)
	out = append(out, oggPage(0x00, 0, serial, 1, tags)...)
	granule := uint64(0)
	seq := uint32(2)
	for i, fr := range frames {
		granule += 1920 // 40ms at 48 kHz (Opus granule)
		ht := byte(0)
		if i == len(frames)-1 {
			ht = 0x04
		}
		out = append(out, oggPage(ht, granule, serial, seq, fr)...)
		seq++
	}
	return out
}

func oggPage(headerType byte, granule uint64, serial, seq uint32, payload []byte) []byte {
	var segs []byte
	left := len(payload)
	off := 0
	for {
		n := left
		if n > 255 {
			n = 255
		}
		segs = append(segs, byte(n))
		off += n
		left -= n
		if left == 0 {
			break
		}
	}
	page := make([]byte, 27+len(segs)+len(payload))
	copy(page[0:], []byte("OggS"))
	page[4] = 0
	page[5] = headerType
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], seq)
	page[26] = byte(len(segs))
	copy(page[27:], segs)
	copy(page[27+len(segs):], payload)
	crc := oggChecksum(page)
	binary.LittleEndian.PutUint32(page[22:26], crc)
	return page
}

func oggChecksum(b []byte) uint32 {
	var crc uint32
	for _, x := range b {
		crc = (crc << 8) ^ oggCRCTab[byte(crc>>24)^x]
	}
	return crc
}

func init() {
	for i := 0; i < 256; i++ {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		oggCRCTab[i] = r
	}
}

var oggCRCTab [256]uint32
