@echo off
cd /d "%~dp0"
if not exist skycontrol.exe (
  echo skycontrol.exe not found. Run BUILD.bat first.
  pause
  exit /b 1
)
echo Starting Sky Control v10...
echo A browser window will open: Main menu and Admin menu.
echo Keep this window open.
echo.
start "" http://127.0.0.1:8080
skycontrol.exe
