# Skrog's one-line installer (#77).
#
#   irm https://wslkit.github.io/skrog/install.ps1 | iex
#
# Downloads a pinned release, verifies it against the release's SHA256SUMS,
# unpacks it, and puts it on your PATH. It does NOT install the engine: that
# provisions a WSL2 distro and downloads a rootfs, which is not something a
# one-liner should do without being asked. `skrog install` is the next step,
# and this script says so when it finishes.
#
# Options, because `irm | iex` cannot pass arguments, come from the environment:
#
#   $env:SKROG_VERSION = '0.3.0'          # default: the newest release
#   $env:SKROG_DIR     = 'C:\tools\skrog' # default: %LOCALAPPDATA%\Programs\skrog
#   $env:SKROG_NO_PATH = '1'               # do not touch PATH
#
# Invoked as a file or a script block, the parameters below work normally:
#
#   & ([scriptblock]::Create((irm https://wslkit.github.io/skrog/install.ps1))) -Version 0.3.0
#
# A CI runner should use the action instead: https://github.com/wslkit/setup-skrog
# It shares this script's release-resolution and verification logic but is
# built for unattended use, with its own inputs and outputs.
[CmdletBinding()]
param(
    [string]$Version = $env:SKROG_VERSION,
    [string]$Dir = $env:SKROG_DIR,
    [switch]$NoPath = [bool]$env:SKROG_NO_PATH
)

$ErrorActionPreference = 'Stop'
# Native tools report failure through exit codes here, not exceptions.
$PSNativeCommandUseErrorActionPreference = $false
$repo = 'wslkit/skrog'

function Say([string]$m) { Write-Host $m }
function Step([string]$m) { Write-Host "  $m" -ForegroundColor DarkGray }

# --- 1. resolve the release --------------------------------------------------

$Version = ($Version -replace '^v', '').Trim()
if (-not $Version) {
    # The repository publishes app releases (v0.3.0) and rootfs releases
    # (rootfs-v29.8.0-1) into one tag namespace, and /releases/latest can
    # return either. "Newest" here means the newest published app release: a
    # v<semver> tag that is not a draft.
    #
    # Stable wins when one exists; the fallback covers the case where none
    # does. That fallback was load-bearing until v0.6.0 -- every release
    # before it was flagged prerelease -- and is now the ordinary path only
    # for someone pointing this at a fork with no stable release.
    #
    # `skrog upgrade --check` deliberately does the OPPOSITE and counts
    # pre-releases, so it can report a preview as available on a machine this
    # script put on the stable build. See internal/upgrade/releases.go and
    # RELEASING.md: first contact should be stable, an explicit "what's new?"
    # should be complete.
    $rels = Invoke-RestMethod -Headers @{ 'User-Agent' = 'skrog-install' } `
        "https://api.github.com/repos/$repo/releases?per_page=50"
    $apps = @($rels | Where-Object { $_.tag_name -match '^v\d+\.\d+\.\d+$' -and -not $_.draft } |
        Sort-Object { [version]($_.tag_name -replace '^v', '') } -Descending)
    $app = @($apps | Where-Object { -not $_.prerelease })[0]
    if (-not $app) { $app = $apps[0] }
    if (-not $app) { throw "no published skrog release found in $repo" }
    $Version = $app.tag_name -replace '^v', ''
}

$arch = if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { 'arm64' } else { 'amd64' }
$base = "https://github.com/$repo/releases/download/v$Version"

# Asset names, newest first. The project was called Hawser through v0.3.1 (#1)
# and a published release's assets are immutable, so installing any version at
# or before that has to ask for the name it actually shipped with. Trying the
# current name first means the fallback costs nothing once it is unreachable.
$candidates = @("skrog_${Version}_windows_$arch.zip", "hawser_${Version}_windows_$arch.zip")

Say ""
Say "Skrog $Version ($arch)"

# --- 2. download and verify --------------------------------------------------

$work = Join-Path ([IO.Path]::GetTempPath()) "skrog-install-$Version"
New-Item -ItemType Directory -Force $work | Out-Null
$sumsPath = Join-Path $work 'SHA256SUMS'

try {
    # SHA256SUMS names whichever asset the release actually published, so it
    # decides which candidate to fetch — no guessing, and no request for a
    # name that cannot exist.
    Invoke-WebRequest "$base/SHA256SUMS" -OutFile $sumsPath
    $sums = Get-Content $sumsPath

    $zip = $null
    foreach ($c in $candidates) {
        if ($sums | Where-Object { $_ -match "\s+\*?$([regex]::Escape($c))\s*$" }) { $zip = $c; break }
    }
    if (-not $zip) {
        throw ("release v$Version publishes none of: " + ($candidates -join ', ') +
            "`nSHA256SUMS lists: " + (($sums | ForEach-Object { ($_ -split '\s+')[-1] }) -join ', '))
    }
    $zipPath = Join-Path $work $zip

    Step "downloading $zip"
    Invoke-WebRequest "$base/$zip" -OutFile $zipPath

    # `<hash>  <file>` per line; a leading * (binary mode) is allowed.
    $entry = $sums |
        Where-Object { $_ -match "\s+\*?$([regex]::Escape($zip))\s*$" } |
        Select-Object -First 1
    if (-not $entry) { throw "SHA256SUMS has no entry for $zip" }
    $expected = ($entry -split '\s+')[0].ToLower()
    $actual = (Get-FileHash $zipPath -Algorithm SHA256).Hash.ToLower()
    if ($actual -ne $expected) {
        throw "checksum mismatch for ${zip}: expected $expected, got $actual"
    }
    Step "verified sha256 $actual"

    # --- 3. unpack -----------------------------------------------------------

    if (-not $Dir) { $Dir = Join-Path $env:LOCALAPPDATA 'Programs\skrog' }
    # An already-running install holds a file lock, which fails the extract
    # with a better error than a half-replaced install but a useless one on
    # its own. Either process name: a pre-rename install is called hawser.
    $running = Get-Process -Name skrog, skrogw, skrogtray, hawser, hawserw, hawsertray -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -and $_.Path.StartsWith($Dir, [StringComparison]::OrdinalIgnoreCase) }
    if ($running) {
        $names = ($running.Name | Sort-Object -Unique) -join ', '
        throw ("Skrog is running from $Dir ($names). " +
            "Stop it first: skrog stop; then quit the tray if it is open.")
    }

    New-Item -ItemType Directory -Force $Dir | Out-Null
    Expand-Archive $zipPath -DestinationPath $Dir -Force

    # v0.3.1 and earlier shipped hawser.exe (#1). Installing one of those is a
    # legitimate thing to ask for, so it is supported and named honestly
    # rather than silently producing a binary the docs never mention.
    $exe = Join-Path $Dir 'skrog.exe'
    $legacy = $false
    if (-not (Test-Path $exe)) {
        $old = Join-Path $Dir 'hawser.exe'
        if (Test-Path $old) { $exe = $old; $legacy = $true }
    }
    if (-not (Test-Path $exe)) { throw "no skrog.exe or hawser.exe after extracting $zip" }
    Step "unpacked to $Dir"
    if ($legacy) {
        Step "note: v$Version predates the Skrog rename - the command is 'hawser', not 'skrog'"
    }
} finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}

# --- 4. PATH -----------------------------------------------------------------

# Read and write the raw user PATH, never the expanded process one. Writing
# back an expanded value is the classic way an installer turns %USERPROFILE%
# in someone's PATH into a literal path that breaks when the profile moves.
function Add-ToUserPath([string]$dir) {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    try {
        $raw = $key.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
        $kind = if ($raw -match '%') { 'ExpandString' } else { 'String' }
        $parts = @($raw -split ';' | Where-Object { $_ })
        if ($parts -contains $dir) { return $false }
        $key.SetValue('Path', (($parts + $dir) -join ';'), $kind)
        return $true
    } finally {
        $key.Dispose()
    }
}

$added = $false
if (-not $NoPath) {
    $added = Add-ToUserPath $Dir
    $env:Path = "$Dir;$env:Path"
}

# --- 5. what now -------------------------------------------------------------

# `skrog version` exits 3 when no engine is installed, which is the expected
# state here and not a failure.
# Collapsed: the version report is tab-aligned, which reads as a gap here.
$reported = ((& $exe --version 2>$null | Select-Object -First 1) -replace '\s+', ' ').Trim()

Say ""
Say "Installed $(if ($reported) { $reported } else { "skrog $Version" })"
Say "  $exe"
if ($added) {
    Say "  added to your PATH (open a new terminal to pick it up)"
} elseif ($NoPath) {
    Say "  PATH not modified (SKROG_NO_PATH)"
} else {
    Say "  already on your PATH"
}

Say ""
Say "Next:"
# The command is whatever actually landed - `skrog` normally, `hawser` when
# a pre-rename version was asked for.
$cmd = [IO.Path]::GetFileNameWithoutExtension($exe)
Say "  $cmd install      provision the engine (downloads a verified rootfs, ~2 min)"
Say "  $cmd doctor       check this machine is ready first"
Say ""
Say "Binaries are not signed yet, so Windows SmartScreen may warn on first run."
Say "Docs: https://wslkit.github.io/skrog/"
Say ""
