@echo off
setlocal
title StarPort Setup
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1"
set "setupResult=%errorlevel%"
echo.
if not "%setupResult%"=="0" echo Setup did not finish successfully. Please read the message above.
pause
exit /b %setupResult%
