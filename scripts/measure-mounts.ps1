# Measure Windows-folder bind-mount performance through a container, so the
# 9p-versus-virtiofs table in docs/vm-sizing.md can be re-taken on any WSL
# build (#523).
#
# The page claims ratios (4.0x read, 3.1x `ls -l`), and a ratio needs BOTH
# transports measured on the same machine and the same WSL build. Switching
# between them needs `wsl --shutdown`, which stops every distro on the machine,
# so this script never does that: it measures whatever the machine is on right
# now and writes a result file, and you flip the setting yourself between the
# two runs, somewhere a shutdown is safe.
#
#   # 1. on the transport you have now (say virtiofs):
#   pwsh -File scripts/measure-mounts.ps1 -Expect virtiofs -Out virtiofs.json
#
#   # 2. skrog config set wsl.virtiofs false ; skrog wsl-config apply ; wsl --shutdown
#   #    (or edit ~/.wslconfig), let the engine come back, then:
#   pwsh -File scripts/measure-mounts.ps1 -Expect 9p -Out 9p.json
#
#   # 3. the table, with the ratios, in the page's format:
#   pwsh -File scripts/measure-mounts.ps1 -Compare 9p.json,virtiofs.json
#
# -Expect is a guard, not a setting. Setting wsl.virtiofs without the shutdown,
# or on a WSL that ignores the key, leaves you on 9p and looks like success in
# ~/.wslconfig (docs/vm-sizing.md says so); the run is refused when the live
# mount is not what you said it would be, rather than recorded under the wrong
# name.
#
# Method, which is the page's: a Windows folder bind-mounted into a container,
# every operation timed INSIDE the container. Per round, one write is made and
# thrown away (a cold VM's first write is not representative), then three are
# kept; reads are warm; the 1000-file operations run once per round. Medians
# are reported with their range, because writes in particular are noisy.
#
# Writes 256 MB under -Dir (default: a temp folder on your profile's drive) and
# removes everything it made. Nothing is installed and no setting is changed.

[CmdletBinding(DefaultParameterSetName = 'Measure')]
param(
    # Measure: the transport you believe the machine is on. The run fails when
    # /proc/mounts disagrees.
    [Parameter(ParameterSetName = 'Measure', Mandatory)]
    [ValidateSet('9p', 'virtiofs')]
    [string]$Expect,

    # Measure: where to write the result.
    [Parameter(ParameterSetName = 'Measure')]
    [string]$Out,

    # Measure: the docker context to run against (default: the current one). A
    # Skrog engine is `skrog`, or `skrog-<name>` for a named engine.
    [Parameter(ParameterSetName = 'Measure')]
    [string]$Context,

    # Measure: the WSL distro whose /mnt/c is read to confirm the transport.
    [Parameter(ParameterSetName = 'Measure')]
    [string]$Distro = 'skrog-engine',

    # Measure: an image with sh, dd, date +%s%N and ls. Must already be
    # pullable; busybox-based images work.
    [Parameter(ParameterSetName = 'Measure')]
    [string]$Image = 'alpine:3.20',

    # Measure: a Windows folder to bind-mount. Must be on the drive you want to
    # measure.
    [Parameter(ParameterSetName = 'Measure')]
    [string]$Dir,

    [Parameter(ParameterSetName = 'Measure')]
    [ValidateRange(1, 10)]
    [int]$Rounds = 3,

    # Compare: two result files, in either order. Prints the page's table.
    [Parameter(ParameterSetName = 'Compare', Mandatory)]
    [string[]]$Compare
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# What one round runs, inside the container. Plain POSIX sh and busybox-safe:
# every figure is a nanosecond delta from `date +%s%N`, so nothing depends on
# how a particular dd formats its summary line.
$script:RoundScript = @'
set -e
cd /m
ns() { date +%s%N; }
w() { t0=$(ns); dd if=/dev/zero of=big bs=1M count=256 conv=fsync 2>/dev/null; t1=$(ns); echo $((t1 - t0)); }

w >/dev/null                       # the cold first write: discarded
echo "write $(w)"
echo "write $(w)"
echo "write $(w)"

cat big >/dev/null                 # warm the cache, then time reads
for i in 1 2 3; do
  t0=$(ns); dd if=big of=/dev/null bs=1M 2>/dev/null; t1=$(ns)
  echo "read $((t1 - t0))"
done
rm -f big

mkdir -p small
t0=$(ns); i=0; while [ $i -lt 1000 ]; do echo x > small/f$i; i=$((i + 1)); done; t1=$(ns)
echo "create $((t1 - t0))"
t0=$(ns); ls -l small >/dev/null; t1=$(ns)
echo "ls $((t1 - t0))"
t0=$(ns); for f in small/*; do cat "$f" >/dev/null; done; t1=$(ns)
echo "readmany $((t1 - t0))"
t0=$(ns); rm -rf small; t1=$(ns)
echo "delete $((t1 - t0))"
'@

function Get-Median([double[]]$v) {
    $s = $v | Sort-Object
    $n = @($s).Count
    if ($n % 2) { return [double]$s[($n - 1) / 2] }
    return ([double]$s[$n / 2 - 1] + [double]$s[$n / 2]) / 2
}

function Invoke-Docker {
    param([string[]]$DockerArgs)
    $a = @()
    if ($script:Context) { $a += @('--context', $script:Context) }
    $a += $DockerArgs
    $out = & docker @a 2>&1
    if ($LASTEXITCODE -ne 0) { throw "docker $($DockerArgs -join ' ') failed:`n$out" }
    return $out
}

function Get-LiveTransport {
    # The same read `skrog doctor` does: /proc/mounts in the running distro, not
    # ~/.wslconfig. `wsl --exec` boots a stopped distro, which is fine here: this
    # script runs against an engine you are using.
    $env:WSL_UTF8 = '1'
    $line = (& wsl.exe -d $Distro -u root --exec sh -c "grep ' /mnt/c ' /proc/mounts" 2>&1) -join "`n"
    if ($LASTEXITCODE -ne 0 -or -not $line) { return $null }
    if ($line -match '\svirtiofs\s') { return 'virtiofs' }
    if ($line -match '\s9p\s') { return '9p' }
    return $null
}

function Get-Machine {
    $m = [ordered]@{
        windows = [Environment]::OSVersion.Version.ToString()
        cores   = [Environment]::ProcessorCount
    }
    try { $m.ramGiB = [math]::Round((Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory / 1GB, 1) } catch {}
    try { $m.cpu = (Get-CimInstance Win32_Processor | Select-Object -First 1).Name.Trim() } catch {}
    try {
        $drive = (Split-Path -Qualifier $script:Dir).TrimEnd(':')
        $disk = Get-Partition -DriveLetter $drive | Get-Disk
        $m.disk = "$($disk.FriendlyName) ($($disk.BusType))"
    } catch {}
    return $m
}

function Invoke-Measure {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw 'no docker CLI on PATH' }
    $script:Context = $Context
    $script:Distro = $Distro

    $live = Get-LiveTransport
    if (-not $live) {
        throw "could not read the live /mnt/c mount of distro '$Distro' (is the engine installed and is -Distro right?)"
    }
    if ($live -ne $Expect) {
        throw "the engine is on $live, not $Expect. Setting wsl.virtiofs needs `wsl --shutdown` to take effect, " +
              "and WSL below 2.9 ignores the key silently. Nothing was measured."
    }

    $own = $false
    if (-not $Dir) {
        $Dir = Join-Path $env:TEMP "skrog-measure-mounts-$PID"
        $own = $true
    }
    New-Item -ItemType Directory -Force -Path $Dir | Out-Null
    $script:Dir = $Dir
    $wslVersion = ((& wsl.exe --version 2>&1) -join ' ') -replace '\0', ''
    $wslVersion = if ($wslVersion -match 'WSL version:\s*([0-9.]+)') { $Matches[1] } else { 'unknown' }
    $server = (Invoke-Docker @('version', '--format', '{{.Server.Version}}') | Select-Object -Last 1).ToString().Trim()

    Write-Host "transport $live, WSL $wslVersion, docker engine $server, $Rounds round(s), image $Image"

    $samples = @{ write = @(); read = @(); create = @(); ls = @(); readmany = @(); delete = @() }
    try {
        for ($r = 1; $r -le $Rounds; $r++) {
            Write-Host "  round $r of $Rounds ..."
            $lines = Invoke-Docker @('run', '--rm', '-v', "${Dir}:/m", $Image, 'sh', '-c', $script:RoundScript)
            foreach ($l in $lines) {
                if ("$l" -match '^(write|read|create|ls|readmany|delete) (\d+)$') {
                    $samples[$Matches[1]] += [double]$Matches[2]
                }
            }
        }
    } finally {
        if ($own) { Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $Dir }
    }

    $expectN = @{ write = 3 * $Rounds; read = 3 * $Rounds; create = $Rounds; ls = $Rounds; readmany = $Rounds; delete = $Rounds }
    foreach ($k in $expectN.Keys) {
        if ($samples[$k].Count -ne $expectN[$k]) {
            throw "expected $($expectN[$k]) '$k' samples and got $($samples[$k].Count); the image may lack date +%s%N or dd"
        }
    }

    # Throughput in MB/s for the 256 MB operations, seconds for the rest.
    $mbps = { param($ns) 256.0 / ($ns / 1e9) }
    $sec = { param($ns) $ns / 1e9 }
    $summary = [ordered]@{}
    foreach ($k in 'write', 'read') {
        $v = @($samples[$k] | ForEach-Object { & $mbps $_ })
        $summary[$k] = [ordered]@{ unit = 'MB/s'; n = $v.Count; median = Get-Median $v; min = ($v | Measure-Object -Minimum).Minimum; max = ($v | Measure-Object -Maximum).Maximum }
    }
    foreach ($k in 'create', 'ls', 'readmany', 'delete') {
        $v = @($samples[$k] | ForEach-Object { & $sec $_ })
        $summary[$k] = [ordered]@{ unit = 's'; n = $v.Count; median = Get-Median $v; min = ($v | Measure-Object -Minimum).Minimum; max = ($v | Measure-Object -Maximum).Maximum }
    }

    $result = [ordered]@{
        transport = $live
        measured  = (Get-Date).ToString('yyyy-MM-dd')
        wsl       = $wslVersion
        engine    = $server
        image     = $Image
        rounds    = $Rounds
        machine   = Get-Machine
        results   = $summary
    }
    $json = $result | ConvertTo-Json -Depth 6
    if ($Out) {
        Set-Content -Path $Out -Value $json -Encoding utf8
        Write-Host "wrote $Out"
    } else {
        $json
    }
}

function Format-Cell($s, [string]$fmt) {
    '{0} ({1}-{2})' -f ($s.median.ToString($fmt)), ($s.min.ToString($fmt)), ($s.max.ToString($fmt))
}

function Invoke-Compare {
    # `pwsh -File` hands a comma list over as ONE string, so split it ourselves.
    $files = @($Compare | ForEach-Object { $_ -split ',' } | Where-Object { $_ })
    if ($files.Count -ne 2) { throw "-Compare takes exactly two result files (got $($files.Count))" }
    $a = Get-Content -Raw $files[0] | ConvertFrom-Json
    $b = Get-Content -Raw $files[1] | ConvertFrom-Json
    $p9 = @($a, $b) | Where-Object { $_.transport -eq '9p' }
    $vf = @($a, $b) | Where-Object { $_.transport -eq 'virtiofs' }
    if (@($p9).Count -ne 1 -or @($vf).Count -ne 1) {
        throw "need exactly one 9p result and one virtiofs result (got $($a.transport) and $($b.transport))"
    }
    $p9 = $p9[0]; $vf = $vf[0]

    # A ratio between different builds or machines is not the page's claim.
    foreach ($k in 'wsl') {
        if ($p9.$k -ne $vf.$k) { throw "the two runs are on different WSL builds ($($p9.$k) and $($vf.$k)); a ratio needs the same one" }
    }
    if ($p9.machine.cpu -ne $vf.machine.cpu -or $p9.machine.windows -ne $vf.machine.windows) {
        throw 'the two runs are from different machines or Windows builds; a ratio needs the same one'
    }

    $rows = @(
        @{ key = 'write';    label = 'write 256 MB (`conv=fsync`)'; fmt = '0';   hi = $true  },
        @{ key = 'read';     label = 'read 256 MB (warm)';          fmt = '0';   hi = $true  },
        @{ key = 'create';   label = 'create 1000 files';           fmt = '0.00'; hi = $false },
        @{ key = 'ls';       label = '`ls -l` 1000 files';          fmt = '0.00'; hi = $false },
        @{ key = 'readmany'; label = 'read 1000 files';             fmt = '0.00'; hi = $false },
        @{ key = 'delete';   label = 'delete 1000 files';           fmt = '0.00'; hi = $false }
    )
    "WSL $($vf.wsl), measured $($vf.measured), engine $($vf.engine), Windows $($vf.machine.windows), $($vf.machine.cores) cores, $($vf.machine.ramGiB) GiB RAM, disk $($vf.machine.disk)"
    ''
    '| | 9p | virtiofs | |'
    '|---|---|---|---|'
    foreach ($r in $rows) {
        $x = $p9.results.($r.key); $y = $vf.results.($r.key)
        $unit = if ($r.hi) { ' MB/s' } else { ' s' }
        # Throughput: higher is better, so the gain is virtiofs/9p. Times: lower
        # is better, so it is 9p/virtiofs. Either way ">1" means virtiofs won.
        $gain = if ($r.hi) { $y.median / $x.median } else { $x.median / $y.median }
        '| {0} | {1}{3} | {2}{3} | {4:0.0}x |' -f $r.label, (Format-Cell $x $r.fmt), (Format-Cell $y $r.fmt), $unit, $gain
    }
    ''
    'Medians, with the range in brackets. Compare each ratio with what the page claims before editing it:'
    '4.0x read, 3.1x `ls -l`, 1.7x delete (2.9.11). A ratio below 1.0x means virtiofs was slower.'
}

if ($PSCmdlet.ParameterSetName -eq 'Compare') { Invoke-Compare } else { Invoke-Measure }
