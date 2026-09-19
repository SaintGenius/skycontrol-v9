@echo off
cd /d "%~dp0"
if not exist skycontrol.exe (
  echo skycontrol.exe not found. Run BUILD.bat first.
  pause
  exit /b 1
)
echo Starting Sky Control v10...
echo Browser: http://127.0.0.1:8080
echo Keep this window open.
echo.
skycontrol.exe