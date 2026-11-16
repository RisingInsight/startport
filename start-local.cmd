@echo off
setlocal
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\local-server.ps1" -Action Start -OpenPanel
set "startupResult=%errorlevel%"
if not "%startupResult%"=="0" pause
exit /b %startupResult%
