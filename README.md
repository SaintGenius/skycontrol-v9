# Sky Control v10

Native SRS listen + **GUID → jet lock**.

## What v10 is

- ATC talks and **hears** on native SRS (your radio PTT)
- First key: lock that SRS GUID to your Tacview jet
- If it cannot lock: `Station calling, say your callsign.`
- Same radio stays that jet; another pilot on the same freq does not steal the conversation

## Install

1. Download ZIP from this repo
2. Copy the `internal` folder into your existing Sky Control folder (overwrite)
3. Also copy `LAUNCH.bat` if you want the v10 banner
4. Keep `config.yaml`, `piper\`, `data\voices\`
5. Run **BUILD.bat**

Startup should show:

```
Sky Control v10
ATC radio: SRS native (stays connected)
SRS native: connected
SRS listen ON
```
