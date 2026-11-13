# run-executor.ps1
# Run on the RDP client machine (10.0.1.106), in the logged-in desktop session.
# Usage:  powershell -ExecutionPolicy Bypass -File .\run-executor.ps1
#         ... -File .\run-executor.ps1 -Explore   # lockdown for `orchestrator explore-*` and `ask`:
#             menu keys only, no typing, no clicking
#         ... -File .\run-executor.ps1 -Task      # `ask` with lookups: menu keys + Tab, typing up to
#             20 characters per call (one lookup value), no clicking
#         ... -File .\run-executor.ps1 -Watch     # `watch-kickbacks --notify-only`: Order List's hotkey,
#             refresh, scrolling and closing only; no Enter, no Tab, no typing, no clicking

param([switch]$Explore, [switch]$Task, [switch]$Watch)

$ErrorActionPreference = "Stop"

# ---- settings ----
$ListenIP  = "192.168.128.235"        # this machine's IP (where the ERP RemoteApp window is)
$Port      = 8765
$AllowFrom = "192.168.126.8,192.168.126.119"   # the orchestrator Mac: wired (en7) and Wi-Fi (en0)
$ErpTitle  = "KINGS RIVER PACKING, LP - FAMOUS (KRP-AVD-Workspace)"         # part of the ERP window title; find it with GET /windows?all=1
# Keep in sync with EXPLORE_KEYS in orchestrator/explore.py.
$ExploreKeys = "alt+f,alt+e,alt+v,alt+t,alt+w,alt+h,up,down,right,home,enter,esc,f1,ctrl+f4,ctrl+a,ctrl+c,ctrl+r,ctrl+tab,left,shift+f1,shift+f2"
# Keep in sync with TASK_KEYS in orchestrator/explore.py.
$TaskKeys = "$ExploreKeys,tab,shift+f3"
# Keep in sync with WATCH_KEYS in orchestrator/explore.py.
$WatchKeys = "shift+f1,ctrl+r,up,down,home,esc,ctrl+f4,ctrl+tab"

# ---- paths ----
$Here      = Split-Path -Parent $MyInvocation.MyCommand.Path
$Exe       = Join-Path $Here "erp-executor.exe"
$TokenFile = Join-Path $Here "executor-token.txt"
$AuditDir  = Join-Path $Here "audit"

if (-not (Test-Path $Exe)) {
    throw "erp-executor.exe not found in $Here. Build it first: go build -o erp-executor.exe ."
}
if ($ErpTitle -eq "CHANGE ME") {
    throw "Edit this script and set `$ErpTitle to part of your ERP window's title."
}

# Generate a random token once and reuse it on later runs.
if (-not (Test-Path $TokenFile)) {
    $bytes = New-Object byte[] 32
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    ([BitConverter]::ToString($bytes) -replace "-", "") | Set-Content -NoNewline $TokenFile
    Write-Host "Generated a new token in $TokenFile -- copy it to the orchestrator machine."
}
$env:EXECUTOR_TOKEN = (Get-Content -Raw $TokenFile).Trim()

$Extra = @()
if ((@($Explore.IsPresent, $Task.IsPresent, $Watch.IsPresent) | Where-Object { $_ }).Count -gt 1) { throw "Use one of -Explore, -Task, -Watch." }
if ($Task) {
    $Extra = @("-allow-keys", $TaskKeys, "-max-text", "20", "-allow-click=false")
    Write-Host "TASK lockdown: keys limited to $TaskKeys; typing up to 20 characters per call; clicking disabled"
}
if ($Explore) {
    $Extra = @("-allow-keys", $ExploreKeys, "-allow-type=false", "-allow-click=false")
    Write-Host "EXPLORE lockdown: keys limited to $ExploreKeys; typing and clicking disabled"
}
if ($Watch) {
    $Extra = @("-allow-keys", $WatchKeys, "-allow-type=false", "-allow-click=false")
    Write-Host "WATCH lockdown: keys limited to $WatchKeys; no Enter, typing or clicking"
}

Write-Host "Starting erp-executor on $($ListenIP):$Port, accepting only $AllowFrom"
& $Exe `
    -title $ErpTitle `
    -addr "$($ListenIP):$Port" `
    -allow-from $AllowFrom `
    -audit-dir $AuditDir `
    @Extra