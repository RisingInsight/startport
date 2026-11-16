@echo off
setlocal
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\local-server.ps1" -Action Stop
set "stopResult=%errorlevel%"
if not "%stopResult%"=="0" pause
exit /b %stopResult%
