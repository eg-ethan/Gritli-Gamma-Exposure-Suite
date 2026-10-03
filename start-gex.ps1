# start-gex.ps1 — one-command live session: core (docker) + C# edge + Chrome.
#
#   .\start-gex.ps1          normal start (no rebuild — run this after every reboot)
#   .\start-gex.ps1 -Build   rebuild BOTH sides first (only after code changes;
#                            the protocol version gate requires core+edge in sync)
#
# Everything configurable lives in the variables below; the watchlist lives in
# watchlist.json (read by BOTH the core and this script, so the GUI rows and
# the edge's --tickers never drift apart).

param([switch]$Build)

# ── variables ──────────────────────────────────────────────────────────────
$Repo        = $PSScriptRoot
$GuiUrl      = "http://localhost:8787"
$CoreAddr    = "127.0.0.1:7878"          # edge -> core (docker port mapping)
$TwsAddr     = "127.0.0.1:7496"          # 7496 live TWS | 7497 paper TWS
$ClientId    = 11
$WatchlistJs = Join-Path $Repo "watchlist.json"
$EdgeExe     = Join-Path $Repo "edge\src\GexEdge\bin\Debug\net8.0\win-x64\gex-edge.exe"
$EdgeProj    = Join-Path $Repo "edge\src\GexEdge\GexEdge.csproj"
$LogDir      = Join-Path $Repo "logs"
$ChromePaths = @(
    "$env:ProgramFiles\Google\Chrome\Application\chrome.exe",
    "${env:ProgramFiles(x86)}\Google\Chrome\Application\chrome.exe",
    "$env:LOCALAPPDATA\Google\Chrome\Application\chrome.exe"
)
# ───────────────────────────────────────────────────────────────────────────

$ErrorActionPreference = "Stop"
Set-Location $Repo

# 1. tickers from the watchlist config (the same file the core serves from)
if (-not (Test-Path $WatchlistJs)) { Write-Error "watchlist.json not found: $WatchlistJs" }
$wl = Get-Content $WatchlistJs -Raw | ConvertFrom-Json
$Tickers = @($wl.tickers | ForEach-Object { if ($_ -is [string]) { $_ } else { $_.ticker } })
if ($Tickers.Count -eq 0) { Write-Error "watchlist.json lists no tickers" }
$TickerCsv = $Tickers -join ","
Write-Host "watchlist: $TickerCsv"

# 2. a stale edge process holds the TWS clientId and makes every reconnect
#    bounce (the s2..s17 session churn, 2026-10-01) — take it down first
$stale = Get-Process "gex-edge" -ErrorAction SilentlyContinue
if ($stale) {
    Write-Host "stopping stale gex-edge (pid $($stale.Id -join ', '))"
    $stale | Stop-Process -Force
    Start-Sleep -Milliseconds 800
}

# 3. the core (docker). Plain up normally; --build only when asked
if ($Build) {
    Write-Host "building the core image..."
    docker compose up -d --build
    if ($LASTEXITCODE -ne 0) { Write-Error "docker compose build failed" }
} else {
    docker compose up -d
    if ($LASTEXITCODE -ne 0) { Write-Error "docker compose up failed (new code? run .\start-gex.ps1 -Build)" }
}

# 4. the edge. Build only when missing or explicitly asked (normal starts skip it)
if ($Build -or -not (Test-Path $EdgeExe)) {
    Write-Host "building the edge..."
    dotnet build $EdgeProj
    if ($LASTEXITCODE -ne 0) { Write-Error "edge build failed" }
}

# 5. wait for the core's GUI, then start the edge with output to a dated log
#    (the log is where connection churn / TWS errors land — check it first)
New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
$EdgeLog = Join-Path $LogDir ("edge-{0}.log" -f (Get-Date -Format "yyyyMMdd-HHmmss"))
$coreOk = $false
foreach ($i in 1..30) {
    try {
        Invoke-WebRequest "$GuiUrl/api/health" -UseBasicParsing -TimeoutSec 2 | Out-Null
        $coreOk = $true
        break
    } catch { Start-Sleep 1 }
}
if (-not $coreOk) { Write-Warning "core GUI not answering yet — starting the edge anyway (it retries)" }

$edgeArgs = @("--core", $CoreAddr, "--tws", $TwsAddr, "--client-id", "$ClientId", "--tickers", $TickerCsv)
Write-Host "starting edge: gex-edge.exe $($edgeArgs -join ' ')"
$procArgs = @{
    FilePath                 = $EdgeExe
    ArgumentList             = $edgeArgs
    WorkingDirectory         = $Repo
    RedirectStandardOutput   = $EdgeLog
    RedirectStandardError    = "$EdgeLog.err"
    WindowStyle              = "Hidden"
}
Start-Process @procArgs

# 6. open the GUI in a clean Chrome app window
$chrome = $ChromePaths | Where-Object { Test-Path $_ } | Select-Object -First 1
if ($chrome) {
    Start-Process -FilePath $chrome -ArgumentList "--app=$GuiUrl"
} else {
    Write-Host "Chrome not found — opening the default browser"
    Start-Process $GuiUrl
}

Write-Host ""
Write-Host "GEX session up: GUI $GuiUrl"
Write-Host "edge log: $EdgeLog (+ .err) — check here first when something looks off"
Write-Host "stop everything: docker compose down  +  Stop-Process -Name gex-edge"
Write-Host "sweeps: GUI 'Run 1 pass' buttons only — nothing sweeps at boot."
