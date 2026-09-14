# nightly-fit.ps1 — the nightly GARCH fit job (architecture.md §2).
#
# Fits GJR-GARCH(1,1) per ticker from a vendor closes CSV and persists
# Params + warm-start State into GARCH_Parameters / GARCH_State, from where
# the app's engines anchor their term structures (app.engineFor).
#
# Ticker→CSV mapping lives in fit-tickers.json next to this script:
#   { "db": "E:/gexProject/core/gex.db",
#     "tickers": { "SPX": "E:/gexdata/SPX_closes.csv", ... } }
# CSV format (any vendor export): one close per line, "date,close" or bare
# "close"; a header row is tolerated (gexctl fit-garch --returns).
#
# Register once (run as the user who owns gex.db):
#   schtasks /Create /TN "GEX nightly GARCH fit" /SC DAILY /ST 18:30 ^
#     /TR "powershell -NoProfile -ExecutionPolicy Bypass -File E:\gexProject\ops\nightly-fit.ps1"
#
# gexctl resolution order: $env:GEXCTL, .\core\gexctl.exe, E:\gexProject\core\gexctl.exe.

$ErrorActionPreference = "Continue"

$here     = Split-Path -Parent $MyInvocation.MyCommand.Path
$config   = Join-Path $here "fit-tickers.json"
$logDir   = Join-Path $here "logs"
$stamp    = Get-Date -Format "yyyyMMdd-HHmmss"

if (-not (Test-Path $config)) {
    Write-Error "missing config: $config (copy the shape from the header comment)"
    exit 1
}
if (-not (Test-Path $logDir)) { New-Item -ItemType Directory -Path $logDir | Out-Null }
$log = Join-Path $logDir "fit-$stamp.log"

$gexctl = $env:GEXCTL
if (-not $gexctl) {
    foreach ($c in @((Join-Path $here "..\core\gexctl.exe"), "E:\gexProject\core\gexctl.exe")) {
        $resolved = [System.IO.Path]::GetFullPath((Join-Path $here $c))
        if (Test-Path $resolved) { $gexctl = $resolved; break }
    }
}
if (-not $gexctl -or -not (Test-Path $gexctl)) {
    Write-Error "gexctl.exe not found — build it: cd core; go build -o gexctl.exe ./cmd/gexctl"
    exit 1
}

$cfg = Get-Content $config -Raw | ConvertFrom-Json
$db = $cfg.db
if (-not $db) { Write-Error 'config needs "db" (path to gex.db)'; exit 1 }

$failed = 0
foreach ($p in $cfg.tickers.PSObject.Properties) {
    $ticker = $p.Name
    $csv = $p.Value
    "[$(Get-Date -Format o)] $ticker  <-  $csv" | Tee-Object -FilePath $log -Append
    if (-not (Test-Path $csv)) {
        "  SKIP: closes CSV not found" | Tee-Object -FilePath $log -Append
        $failed++
        continue
    }
    & $gexctl fit-garch --ticker $ticker --returns $csv --db $db --json *>> $log
    if ($LASTEXITCODE -ne 0) {
        "  FAILED (exit $LASTEXITCODE)" | Tee-Object -FilePath $log -Append
        $failed++
    } else {
        "  OK — persisted to $db" | Tee-Object -FilePath $log -Append
    }
}

"[$(Get-Date -Format o)] done; failures: $failed" | Tee-Object -FilePath $log -Append
exit ([Math]::Min($failed, 1))
