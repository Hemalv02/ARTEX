@echo off
rem Switch the console to UTF-8; otherwise the Chinese text in this file
rem shows up as mojibake on a GBK terminal.
chcp 65001 >nul 2>&1
rem ARTEX daemon startup script (Windows)
rem
rem Usage:
rem   start.bat                  Run in the foreground (Ctrl-C to stop)
rem   start.bat -addr :9000      Extra args are passed through to artex verbatim
rem
rem It only does one thing: runs artex.exe, and after the process exits,
rem decides whether to relaunch it based on the exit code.
rem
rem   0      User-initiated normal stop   -> Exit the loop
rem   75     Program-requested restart    -> Rerun immediately ("One-click update"
rem                                         or "Rollback" was clicked in the UI)
rem   other  Crash                        -> Rerun after backoff (1->2->4... up to 60s)
rem
rem Download, SHA256 verification, and binary swap are NOT done here — all of
rem that is performed by artex itself at startup (the selfupdate package).
rem The script stays foolproof; see the comment at the top of start.sh for details.

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] Executable not found: %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] Normal exit
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem Update/rollback is staged: after relaunch, artex will perform the
	rem binary swap at startup.
	echo [artex] Restart requested (applying new version)...
	set /a delay=1
	goto loop
)

echo [artex] Abnormal exit ^(code=!code!^), restarting in !delay!s 1>&2
rem `timeout` fails in a redirected console, so fall back to `ping`
rem (sleeping N seconds requires N+1 pings).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
