package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Field is a GUI setting. The panel renders this list — it does not know
// about Piper, Whisper, Tacview, or SRS internals. Add a field here and
// the form updates; engines can change without touching HTML.
type Field struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Group   string   `json:"group"`
	Kind    string   `json:"kind"` // text, password, number, bool, select, list
	Help    string   `json:"help"`
	Options []string `json:"options,omitempty"`
}

func Schema() []Field {
	return []Field{
		{Key: "default-callsign", Label: "Default station name", Group: "Station", Kind: "text", Help: "Used when the airfield is unknown. Usually Tower."},

		{Key: "telemetry-address", Label: "Telemetry address", Group: "Position", Kind: "text", Help: "Where live aircraft positions arrive. Default 127.0.0.1:42674."},
		{Key: "telemetry-password", Label: "Telemetry password", Group: "Position", Kind: "password", Help: "Leave blank unless your telemetry host uses one."},

		{Key: "srs-address", Label: "Radio server", Group: "Radio", Kind: "text", Help: "SimpleRadio host:port. Default 127.0.0.1:5002."},
		{Key: "srs-password", Label: "Radio password", Group: "Radio", Kind: "password", Help: "SRS External AWACS (EAM) password if the server uses one. Blank is fine for a local server."},
		{Key: "srs-coalition", Label: "Coalition", Group: "Radio", Kind: "select", Options: []string{"1", "2"}, Help: "1 = red, 2 = blue."},
		{Key: "srs-external-audio", Label: "ExternalAudio path", Group: "Radio", Kind: "text", Help: "Full path to DCS-SR-ExternalAudio.exe."},
		{Key: "srs-frequencies", Label: "Transmit frequencies", Group: "Radio", Kind: "list", Help: "One per line, e.g. 305.0AM. ATC uses the first unless the call has a freq."},

		{Key: "voice-provider", Label: "Voice engine", Group: "Voice", Kind: "select", Options: []string{"piper", "stub"}, Help: "piper = local voice. stub = log only."},
		{Key: "voice-name", Label: "Voice name", Group: "Voice", Kind: "text", Help: "Piper alias, e.g. american-military-male."},
		{Key: "voice-speed", Label: "Voice speed", Group: "Voice", Kind: "number", Help: "1.0 is normal. Lower is slower."},
		{Key: "voice-volume", Label: "Voice volume", Group: "Voice", Kind: "number", Help: "1.0 is full."},

		{Key: "ptt-key", Label: "PTT button", Group: "Microphone", Kind: "text", Help: "PAD1 = Xbox A / Radio 1. OFF = type only. ALWAYS = open mic."},

		{Key: "ai-enabled", Label: "AI controller", Group: "Controller", Kind: "bool", Help: "On = ChatGPT ATC. Off = keyword backup only."},
		{Key: "ai-api-key", Label: "AI API key", Group: "Controller", Kind: "password", Help: "OpenAI secret key. Stored only in config.yaml on this PC."},
		{Key: "ai-base-url", Label: "AI endpoint", Group: "Controller", Kind: "text", Help: "OpenAI-compatible API root."},
		{Key: "ai-model", Label: "AI model", Group: "Controller", Kind: "text", Help: "e.g. gpt-4o-mini."},

		{Key: "wind-file", Label: "DCS wind file", Group: "Runway", Kind: "text", Help: "Leave blank to auto-find Saved Games\\DCS\\SkyControl\\wind.txt from the DCS hook."},
		{Key: "wind-from", Label: "Wind from (deg)", Group: "Runway", Kind: "number", Help: "Fallback if the DCS hook is off. -1 = unused. 270 = west wind."},
		{Key: "wind-speed-kt", Label: "Wind speed (kt)", Group: "Runway", Kind: "number", Help: "Fallback knots. DCS switches runway at 6 kt. -1 = unused."},

		{Key: "log-level", Label: "Log level", Group: "System", Kind: "select", Options: []string{"debug", "info", "warn", "error"}, Help: "Written to skycontrol.log."},
	}
}

func Defaults() map[string]any {
	return map[string]any{
		"default-callsign":    "Tower",
		"telemetry-address":   "127.0.0.1:42674",
		"telemetry-password":  "",
		"srs-address":         "127.0.0.1:5002",
		"srs-password":        "",
		"srs-coalition":       2,
		"srs-external-audio":  `C:\Program Files\DCS-SimpleRadio-Standalone\ExternalAudio\DCS-SR-ExternalAudio.exe`,
		"srs-frequencies":     []string{"305.0AM", "261.0AM", "251.0AM"},
		"voice-provider":      "piper",
		"voice-name":          "american-military-male",
		"voice-speed":         1.0,
		"voice-volume":        1.0,
		"ptt-key":             "PAD1",
		"ai-enabled":          true,
		"ai-api-key":          "",
		"ai-base-url":         "https://api.openai.com/v1",
		"ai-model":            "gpt-4o-mini",
		"wind-file":           "",
		"wind-from":           -1.0,
		"wind-speed-kt":       -1.0,
		"log-level":           "info",
	}
}

func ValuesFrom(c *Config) map[string]any {
	if c == nil {
		return Defaults()
	}
	return map[string]any{
		"default-callsign":   c.DefaultCallsign,
		"telemetry-address":  c.TelemetryAddress,
		"telemetry-password": c.TelemetryPassword,
		"srs-address":        c.SRSAddress,
		"srs-password":       c.SRSPassword,
		"srs-coalition":      c.SRSCoalition,
		"srs-external-audio": c.SRSExternalAudio,
		"srs-frequencies":    c.SRSFrequencies,
		"voice-provider":     c.VoiceProvider,
		"voice-name":         c.VoiceName,
		"voice-speed":        c.VoiceSpeed,
		"voice-volume":       c.VoiceVolume,
		"ptt-key":            c.PTTKey,
		"ai-enabled":         c.AIEnabled,
		"ai-api-key":         c.AIAPIKey,
		"ai-base-url":        c.AIBaseURL,
		"ai-model":           c.AIModel,
		"wind-file":          c.WindFile,
		"wind-from":          c.WindFrom,
		"wind-speed-kt":      c.WindSpeedKt,
		"log-level":          c.LogLevel,
	}
}

func WriteYAML(path string, values map[string]any) error {
	merged := Defaults()
	for k, v := range values {
		merged[k] = v
	}
	var b strings.Builder
	b.WriteString("# Sky Control — written by the Settings panel.\n")
	b.WriteString("# Restart Sky Control after saving.\n")
	cur := ""
	for _, f := range Schema() {
		if f.Group != cur {
			cur = f.Group
			fmt.Fprintf(&b, "\n# %s\n", cur)
		}
		v := merged[f.Key]
		switch f.Kind {
		case "bool":
			fmt.Fprintf(&b, "%s: %s\n", f.Key, boolStr(v))
		case "number":
			fmt.Fprintf(&b, "%s: %s\n", f.Key, numStr(v))
		case "list":
			fmt.Fprintf(&b, "%s:\n", f.Key)
			for _, item := range asList(v) {
				fmt.Fprintf(&b, "  - %q\n", item)
			}
		default:
			fmt.Fprintf(&b, "%s: %q\n", f.Key, fmt.Sprint(v))
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func RestoreDefaults(path string) error {
	if _, err := os.Stat(path); err == nil {
		_ = os.WriteFile(path+".bak", mustRead(path), 0644)
	}
	return WriteYAML(path, Defaults())
}

func mustRead(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

func boolStr(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		if t == "true" || t == "1" || t == "on" {
			return "true"
		}
		return "false"
	}
	return "false"
}

func numStr(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case string:
		return t
	}
	return fmt.Sprint(v)
}

func asList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			s := strings.TrimSpace(fmt.Sprint(x))
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		var out []string
		for _, line := range strings.Split(t, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		return out
	}
	return nil
}
