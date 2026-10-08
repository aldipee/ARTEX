@echo off
rem Switch the console to UTF-8 so non-ASCII characters don't garble in a GBK terminal.
chcp 65001 >nul 2>&1
rem ARTEX daemon start script (Windows)
rem
rem Usage:
rem   start.bat                  run in foreground (Ctrl-C to stop)
rem   start.bat -addr :9000      extra args passed through to artex as-is
rem
rem It does one thing: run artex.exe, then decide by its exit code whether to relaunch after it exits.
rem
rem   0      user stopped normally     -> exit loop
rem   75     program requested restart -> rerun immediately (user clicked "one-click update" or "rollback" on the page)
rem   other  crash             -> rerun with backoff (1->2->4…up to 60 seconds)
rem
rem Download, SHA256 verification, and binary swap are not done here; artex handles them itself at startup
rem (selfupdate package). The script stays dumb; see the notes at the top of start.sh.

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] executable not found: %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] exited normally
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem Update/rollback ready: after relaunch, artex completes the swap at startup.
	echo [artex] restart requested (applying new version)…
	set /a delay=1
	goto loop
)

echo [artex] abnormal exit ^(code=!code!^), restarting in !delay!s 1>&2
rem timeout fails in a redirected console, so fall back to ping (delay N seconds needs N+1 pings).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
