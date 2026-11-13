@echo off
rem Leave Remote Desktop WITHOUT locking this PC, so the executor can keep driving FAMOUS.
rem
rem Use this instead of closing the Remote Desktop window. It hands this Windows session
rem to the VM's own console: Remote Desktop disconnects, but the session stays signed in and
rem unlocked, and FAMOUS keeps running. Remote Desktop in again later and you get the same
rem session back.
rem
rem Must run as administrator: right-click > Run as administrator (or make a shortcut with
rem "Run as administrator" ticked under Advanced).
rem
rem Afterwards the screen size is the VM console's display resolution, not your Remote
rem Desktop window's: set the tall resolution on the VM's display (Hyper-V: Set-VMVideo,
rem VMware: VM display settings), not in the .rdp file.

for /f "skip=1 tokens=3" %%s in ('query user %USERNAME%') do (
    %windir%\System32\tscon.exe %%s /dest:console
)
