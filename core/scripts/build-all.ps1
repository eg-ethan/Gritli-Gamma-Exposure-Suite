# Build GEX Suite binaries for every supported platform into .\dist.
# Pure Go (modernc SQLite) - one machine cross-compiles all targets, no
# per-OS toolchains, no installers. Users just run the binary.
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

$targets = @(
    @{ os = "windows"; arch = "amd64"; ext = ".exe" },
    @{ os = "linux";   arch = "amd64"; ext = "" },
    @{ os = "linux";   arch = "arm64"; ext = "" },
    @{ os = "darwin";  arch = "amd64"; ext = "" },
    @{ os = "darwin";  arch = "arm64"; ext = "" }
)

if (Test-Path "dist") { Remove-Item -Recurse -Force "dist" }
New-Item -ItemType Directory -Force dist | Out-Null

$env:CGO_ENABLED = "0"
foreach ($t in $targets) {
    $dir = "gexsuite-$($t.os)-$($t.arch)"
    Write-Host "-> $dir"
    New-Item -ItemType Directory -Force (Join-Path dist $dir) | Out-Null
    $env:GOOS = $t.os
    $env:GOARCH = $t.arch
    go build -trimpath -ldflags "-s -w" -o (Join-Path dist "$dir\gexctl$($t.ext)") ./cmd/gexctl
    if (Test-Path "README-GUI.md") { Copy-Item "README-GUI.md" (Join-Path dist "$dir\") }
}

Remove-Item Env:GOOS, Env:GOARCH

# zip archives for download/transfer
foreach ($d in Get-ChildItem dist -Directory) {
    Compress-Archive -Path $d.FullName -DestinationPath (Join-Path dist "$($d.Name).zip") -Force
}

Write-Host "`ndone:"
Get-ChildItem dist | Select-Object -ExpandProperty Name
