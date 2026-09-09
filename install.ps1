<#
.SYNOPSIS
  FivePanel agent installer for Windows (x64).

.DESCRIPTION
  Downloads the release archive from GitHub Releases, verifies its checksum,
  installs fivepanel-agent.exe under %ProgramFiles%\FivePanel, adds that
  folder to the machine PATH, runs `fivepanel-agent setup` when -Token and
  -Db are given, and installs + starts the Windows service. Read it before
  you run it. Requires an elevated PowerShell.

.EXAMPLE
  irm https://raw.githubusercontent.com/5panel/agent/main/install.ps1 | iex

.EXAMPLE
  & ([scriptblock]::Create((irm https://raw.githubusercontent.com/5panel/agent/main/install.ps1))) `
    -Token fp_live_... -Db "mysql://fivepanel_ro:pass@127.0.0.1:3306/qbcore"
#>
#Requires -RunAsAdministrator
param(
  [string]$Token = "",
  [string]$Db = "",
  [string]$Engine = "",
  [string]$Gateway = "",
  [string]$Version = "latest",
  [switch]$NoService
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# Piped through Invoke-Expression, an error would only point at the "iex"
# on the caller's command line. Name the failing statement instead.
trap {
  Write-Host ""
  Write-Host "install.ps1 failed: $($_.Exception.Message)" -ForegroundColor Red
  Write-Host ($_.InvocationInfo.PositionMessage) -ForegroundColor DarkGray
  break
}

$repo = "5panel/agent"
$installDir = Join-Path $env:ProgramFiles "FivePanel"
$configPath = Join-Path $env:ProgramData "FivePanel\config.yaml"
$exe = Join-Path $installDir "fivepanel-agent.exe"

# PROCESSOR_ARCHITEW6432 is set when a 32-bit PowerShell runs on 64-bit
# Windows; PROCESSOR_ARCHITECTURE covers the rest. Both exist on every
# Windows PowerShell, unlike RuntimeInformation, which is missing on older
# .NET Framework builds.
$arch = "$env:PROCESSOR_ARCHITEW6432"
if (-not $arch) { $arch = "$env:PROCESSOR_ARCHITECTURE" }
switch ($arch.ToUpper()) {
  "AMD64" { $goarch = "amd64" }
  default { throw "unsupported architecture: '$arch' (releases are built for x64)" }
}

$archive = "fivepanel-agent_windows_$goarch.zip"
if ($Version -eq "latest") {
  $base = "https://github.com/$repo/releases/latest/download"
} else {
  $base = "https://github.com/$repo/releases/download/$Version"
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("fivepanel-agent-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "downloading $base/$archive"
  Invoke-WebRequest -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive) -UseBasicParsing
  Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp "checksums.txt") -UseBasicParsing

  $line = Get-Content (Join-Path $tmp "checksums.txt") | Where-Object { $_ -match "\s$([regex]::Escape($archive))$" } | Select-Object -First 1
  $expected = "$line" -replace "\s.*$", ""
  $actual = "$((Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash)"
  if (-not $expected -or $expected.ToLower() -ne $actual.ToLower()) {
    throw "checksum mismatch for $archive"
  }
  Write-Host "checksum ok"

  Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force
  New-Item -ItemType Directory -Path $installDir -Force | Out-Null

  # Stop a running service before replacing the binary.
  if (Get-Service -Name "fivepanel-agent" -ErrorAction SilentlyContinue) {
    Stop-Service -Name "fivepanel-agent" -ErrorAction SilentlyContinue
  }
  Copy-Item -Path (Join-Path $tmp "fivepanel-agent.exe") -Destination $exe -Force
  Write-Host "installed $exe ($(& $exe version))"

  $machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
  if (($machinePath -split ";") -notcontains $installDir) {
    [Environment]::SetEnvironmentVariable("Path", "$machinePath;$installDir", "Machine")
    Write-Host "added $installDir to the machine PATH (open a new terminal to use it)"
  }
  $env:Path = "$env:Path;$installDir"
}
finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

if ($Token -and $Db) {
  $args = @("setup", "--token", $Token, "--db", $Db, "--config", $configPath, "--yes")
  if ($Engine) { $args += @("--engine", $Engine) }
  if ($Gateway) { $args += @("--gateway", $Gateway) }
  & $exe @args
  if ($LASTEXITCODE -ne 0) { throw "setup failed (exit $LASTEXITCODE)" }
} elseif (-not (Test-Path $configPath)) {
  Write-Host ""
  Write-Host "next: fivepanel-agent setup   (in an elevated terminal)"
  return
}

if (-not $NoService -and (Test-Path $configPath)) {
  if (Get-Service -Name "fivepanel-agent" -ErrorAction SilentlyContinue) {
    & $exe service start
  } else {
    & $exe service install --config $configPath
    & $exe service start
  }
  & $exe service status
  Write-Host "logs: Event Viewer > Windows Logs > Application (source fivepanel-agent)"
}
