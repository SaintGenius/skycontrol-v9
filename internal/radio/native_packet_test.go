package radio

import (
	"bytes"
	"testing"
)

func TestVoicePacketRoundTrip(t *testing.T) {
	guid := []byte("SkyControlGUID1234567X") // 22
	if len(guid) != 22 {
		t.Fatalf("guid len %d", len(guid))
	}
	opus := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 20)
	freqs := []srsFreq{{Hz: 261_000_000, Mod: 0}}
	raw := encodeVoicePacket(opus, freqs, 100000002, 42, guid)
	pkt, err := decodeVoicePacket(raw)
	if err != nil {
		t.Fatal(err)
	}
	if pkt.PacketID != 42 {
		t.Fatalf("packet id %d", pkt.PacketID)
	}
	if !bytes.Equal(pkt.Audio, opus) {
		t.Fatalf("audio mismatch")
	}
	if len(pkt.Freqs) != 1 || pkt.Freqs[0].Hz != 261_000_000 {
		t.Fatalf("freq %+v", pkt.Freqs)
	}
	if string(pkt.OriginGUID) != string(guid) {
		t.Fatalf("guid %q", pkt.OriginGUID)
	}
}

func TestOggOpusHasPages(t *testing.T) {
	frames := [][]byte{bytes.Repeat([]byte{9}, 40), bytes.Repeat([]byte{8}, 40)}
	ogg := buildOggOpus(frames, 16000)
	if !bytes.Contains(ogg, []byte("OpusHead")) || !bytes.Contains(ogg, []byte("OggS")) {
		t.Fatalf("bad ogg")
	}
	pkts := extractOpusPackets(ogg)
	if len(pkts) != 2 {
		t.Fatalf("got %d packets", len(pkts))
	}
}
