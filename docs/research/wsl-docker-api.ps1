<#
.SYNOPSIS
  Exposes the wslc Docker Engine API on tcp://127.0.0.1:2375 and mirrors
  `docker run -p` port bindings to Windows.

.DESCRIPTION
  Actions:

    start   Create the relay (network, back and front socat containers) and
            launch the port watcher in a hidden background process.
            Idempotent: only what is missing is created.
    stop    Stop the watcher, remove all port sidecars and the relay.
            Your own containers are left alone.
    status  Show relay, watcher and sidecar state.

  Then point any Docker client at it:

    $env:DOCKER_HOST = 'tcp://127.0.0.1:2375'

  HOW IT WORKS

  Background: wslc runs dockerd inside a WSL VM. Its API socket,
  /var/run/docker.sock, exists only inside that VM; Windows has no path to it.
  Two wslc rules make it hard to expose:

    1. Only containers created by the wslc CLI get a Windows port relay.
       `wslc run -p W:C` makes wslcsession publish C on a VM-side port V and
       relay Windows W -> VM V, recording it in the container's
       com.microsoft.wsl.container.metadata label. Containers created through
       the engine API get no relay; their -p binds only inside the VM.
    2. `wslc run -v` resolves bind-mount paths against Windows, so a wslc CLI
       container cannot mount the VM's /var/run/docker.sock.

  Each rule blocks a single container from doing both jobs, so the relay uses
  two, chained over a wslc-created bridge network:

    Windows 127.0.0.1:2375
       |   wslcsession relay (Windows -> VM port)
       v
    wslc-engine-front      created with `wslc run -p`; so it gets the relay
       |   socat TCP -> wslc-engine-back:2376 (container DNS on the network)
       v
    wslc-engine-back       created with the VM's own docker CLI via
       |                   `wslc system session run`, so -v resolves in the VM
       |   socat TCP -> UNIX socket
       v
    /var/run/docker.sock   dockerd

  The network is created with `wslc network create`, because wslc cannot see
  networks created through the engine API and `wslc run --network` would
  fail with WSLC_E_NETWORK_NOT_FOUND.

  PORT WATCHER

  Rule 1 also applies to every container you start through DOCKER_HOST:
  `docker run -p 8080:80` binds 8080 inside the VM only. The watcher fixes that
  by creating, for each published port, a wslc CLI "sidecar" that does get a
  Windows relay and forwards into the target:

    Windows 127.0.0.1:8080 -> wslcpub-<id>-8080-tcp (socat) -> <target>:80

  The watcher is this script running with the internal `watch` action. It:

    - follows the engine's /events stream (container start, die, destroy);
    - on start, reads NetworkSettings.Ports, so random ports (-p 80, -P) use
      the host port Docker actually assigned;
    - attaches the target to the relay network if needed, so the sidecar can
      reach it by name;
    - creates one sidecar per binding with `wslc run -d -p ...`, labelled
      wslc.portwatch.target=<container id>;
    - on die/destroy removes the target's sidecars with `wslc remove`, so
      wslcsession tears its relay down cleanly;
    - skips containers created by the wslc CLI (they already have a relay,
      and this includes the sidecars) and --network host containers;
    - on startup, maps running containers without a sidecar and removes
      sidecars whose target is gone, then subscribes to events from just
      before that sync so nothing started in between is missed;
    - if the API stops answering (e.g. the wslc session restarted), rebuilds
      the relay and resyncs.

  The watcher talks to the engine over the REST API directly, so it needs no
  docker.exe; only wslc and pwsh.

  BEHAVIOUR AND LIMITS

    - Ports bound to 0.0.0.0 or :: (the docker default) are published on
      Windows 127.0.0.1 only. An explicit host IP (-p 192.168.1.5:8080:80) is
      passed through to `wslc run -p` unchanged (not tested).
    - All socat processes use a 256 KiB buffer (-b 262144); socat's 8 KiB
      default cut relayed throughput to about a third of a native wslc
      publish.
    - Each sidecar is one small container (about 1-2 MiB RAM idle) and adds
      about 0.5 s to container start. socat forks a process per connection.
    - Sidecars appear only after the container starts (about 1 s later in
      testing), so a client that connects immediately may be refused.
    - SECURITY: 2375 is the unauthenticated Engine API. Anything that can
      reach it has root in the wslc VM. It is bound to 127.0.0.1, which
      limits it to local Windows processes; keep it that way.
    - `wslc system session run` fails with ERROR_INVALID_HANDLE when stdin
      is the null device, so the script feeds it an empty file.
    - `stop` keeps the network if your containers are still attached to it
      (the watcher attaches published containers); `start` reuses it.
    - Nothing starts this automatically at logon.

  Files: %LOCALAPPDATA%\wsl-docker-api\ (watcher.pid, watcher.log).

.PARAMETER Action
  start, stop or status. `watch` is internal: the background watcher loop.

.PARAMETER Port
  Windows loopback port for the Engine API. Default 2375.

.EXAMPLE
  .\wsl-docker-api.ps1 start
  $env:DOCKER_HOST = 'tcp://127.0.0.1:2375'
  docker run -d -p 8080:80 nginx      # reachable at http://127.0.0.1:8080

.EXAMPLE
  .\wsl-docker-api.ps1 status

.EXAMPLE
  .\wsl-docker-api.ps1 stop
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('start', 'stop', 'status', 'watch')]
    [string]$Action = 'status',
    [int]$Port = 2375
)

$ErrorActionPreference = 'Stop'

$Network     = 'wslc-docker-api'
$Front       = 'wslc-engine-front'
$Back        = 'wslc-engine-back'
$Image       = 'alpine/socat'
$TargetLabel = 'wslc.portwatch.target'
$WslcLabel   = 'com.microsoft.wsl.container.metadata'
$Api         = "http://127.0.0.1:$Port"
$StateDir    = Join-Path $env:LOCALAPPDATA 'wsl-docker-api'
$PidFile     = Join-Path $StateDir 'watcher.pid'
$LogFile     = Join-Path $StateDir 'watcher.log'
$EmptyFile   = Join-Path $StateDir 'empty'

New-Item -ItemType Directory -Force $StateDir | Out-Null
if (-not (Test-Path $EmptyFile)) { New-Item -ItemType File $EmptyFile | Out-Null }

function Log($msg) {
    $line = '{0:yyyy-MM-dd HH:mm:ss} {1}' -f (Get-Date), $msg
    if ($Action -eq 'watch') { Add-Content $LogFile $line } else { Write-Host $line }
}

# ---------------------------------------------------------------- helpers

# `wslc system session run` fails with ERROR_INVALID_HANDLE when stdin is the
# null device, so give it a real (empty) file as stdin.
function Invoke-Vm([string[]]$Arguments) {
    $out = Join-Path $StateDir 'vm.out'
    $err = Join-Path $StateDir 'vm.err'
    $p = Start-Process wslc -ArgumentList (@('system', 'session', 'run') + $Arguments) `
        -NoNewWindow -Wait -PassThru -RedirectStandardInput $EmptyFile `
        -RedirectStandardOutput $out -RedirectStandardError $err
    [pscustomobject]@{
        ExitCode = $p.ExitCode
        Output   = ((Get-Content $out, $err -ErrorAction SilentlyContinue) -join "`n").Trim()
    }
}

function Invoke-Api([string]$Method, [string]$Path, $Body) {
    $req = @{ Method = $Method; Uri = "$Api$Path"; TimeoutSec = 10 }
    if ($null -ne $Body) { $req.Body = ($Body | ConvertTo-Json -Compress); $req.ContentType = 'application/json' }
    $r = Invoke-RestMethod @req
    $r      # re-emit so JSON arrays are enumerated; Invoke-RestMethod writes them as one object
}

function Test-Api {
    try { (Invoke-RestMethod "$Api/_ping" -TimeoutSec 3) -eq 'OK' } catch { $false }
}

function Get-Filter([hashtable]$f) { [uri]::EscapeDataString(($f | ConvertTo-Json -Compress)) }

function Get-WatcherProcess {
    if (-not (Test-Path $PidFile)) { return $null }
    $id = [int](Get-Content $PidFile)
    $p = Get-Process -Id $id -ErrorAction SilentlyContinue
    if ($p -and $p.ProcessName -match 'pwsh|powershell') { $p } else { $null }
}

# ---------------------------------------------------------------- relay

function Initialize-Relay {
    # The network must be created by wslc: wslc cannot see engine-created networks.
    $nets = wslc network list 2>&1 | Out-String
    if ($nets -notmatch "\s$Network\s") {
        wslc network create $Network | Out-Null
        Log "created network $Network"
    }

    # Back: created by the VM's docker CLI so it can bind-mount the guest socket.
    $state = (Invoke-Vm @('docker', 'inspect', '-f', '{{.State.Running}}', $Back)).Output
    if ($state -eq 'false') {
        Invoke-Vm @('docker', 'start', $Back) | Out-Null
        Log "started $Back"
    } elseif ($state -ne 'true') {
        $r = Invoke-Vm @('docker', 'run', '-d', '--name', $Back, '--restart', 'unless-stopped',
            '--network', $Network, '-v', '/var/run/docker.sock:/var/run/docker.sock',
            $Image, '-b', '262144', 'TCP-LISTEN:2376,fork,reuseaddr', 'UNIX-CONNECT:/var/run/docker.sock')
        if ($r.ExitCode -ne 0) { throw "creating $Back failed: $($r.Output)" }
        Log "created $Back"
    }

    # Front: created by the wslc CLI so wslc relays Windows loopback to it.
    if (-not (Test-Api)) {
        wslc remove -f $Front *> $null
        wslc run -d --name $Front --network $Network -p "127.0.0.1:${Port}:2375" `
            $Image -b 262144 TCP-LISTEN:2375,fork,reuseaddr "TCP:${Back}:2376" | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "creating $Front failed" }
        Log "created $Front on 127.0.0.1:$Port"
    }

    $deadline = (Get-Date).AddSeconds(20)
    while (-not (Test-Api)) {
        if ((Get-Date) -gt $deadline) { throw "Engine API did not answer on $Api" }
        Start-Sleep -Milliseconds 500
    }
}

function Remove-Relay {
    wslc remove -f $Front *> $null
    Invoke-Vm @('docker', 'rm', '-f', $Back) | Out-Null
    wslc network remove $Network *> $null
    if ($LASTEXITCODE -ne 0) { Log "network $Network kept: containers are still attached" }
}

# ---------------------------------------------------------------- sidecars

function Get-Sidecars([string]$TargetId) {
    $label = if ($TargetId) { "$TargetLabel=$TargetId" } else { $TargetLabel }
    $f = Get-Filter @{ label = @($label) }
    Invoke-Api GET "/containers/json?all=1&filters=$f" | ForEach-Object {
        [pscustomobject]@{ Name = $_.Names[0].TrimStart('/'); Target = $_.Labels.$TargetLabel; Ports = $_.Ports }
    }
}

function Remove-Sidecars([string]$TargetId) {
    foreach ($s in Get-Sidecars $TargetId) {
        wslc remove -f $s.Name *> $null
        Log "removed $($s.Name)"
    }
}

function Add-Sidecars([string]$Id) {
    try { $c = Invoke-Api GET "/containers/$Id/json" } catch { return }

    # wslc CLI containers (including our sidecars) already have Windows relays.
    if ($c.Config.Labels -and $c.Config.Labels.PSObject.Properties[$WslcLabel]) { return }
    if ($c.HostConfig.NetworkMode -eq 'host') { return }
    $ports = $c.NetworkSettings.Ports
    if (-not $ports) { return }

    $bindings = foreach ($p in $ports.PSObject.Properties) {
        if (-not $p.Value) { continue }                     # exposed, not published
        $cport, $proto = $p.Name -split '/'
        foreach ($b in $p.Value) {
            if (-not $b.HostPort) { continue }
            $ip = if ($b.HostIp -and $b.HostIp -notin '0.0.0.0', '::') { $b.HostIp } else { '127.0.0.1' }
            if ($ip -like '*:*') { continue }               # IPv6 twin of the same binding
            [pscustomobject]@{ Ip = $ip; HostPort = $b.HostPort; Port = $cport; Proto = $proto }
        }
    }
    $bindings = $bindings | Sort-Object Ip, HostPort, Proto -Unique
    if (-not $bindings) { return }

    # The sidecar reaches the target by name over the relay network.
    if (-not $c.NetworkSettings.Networks.PSObject.Properties[$Network]) {
        try { Invoke-Api POST "/networks/$Network/connect" @{ Container = $Id } | Out-Null }
        catch { Log "cannot attach $($c.Name) to ${Network}: $_"; return }
    }
    $target   = $c.Name.TrimStart('/')
    $short    = $Id.Substring(0, 12)
    $existing = @(Get-Sidecars $Id | ForEach-Object Name)

    foreach ($b in $bindings) {
        $name = "wslcpub-$short-$($b.HostPort)-$($b.Proto)"
        if ($name -in $existing) { continue }        # replayed event, already mapped
        if ($b.Proto -eq 'udp') {
            $listen = "UDP-RECVFROM:$($b.Port),fork,reuseaddr"; $connect = "UDP-SENDTO:${target}:$($b.Port)"
            $pub = "$($b.Ip):$($b.HostPort):$($b.Port)/udp"
        } else {
            $listen = "TCP-LISTEN:$($b.Port),fork,reuseaddr";   $connect = "TCP:${target}:$($b.Port)"
            $pub = "$($b.Ip):$($b.HostPort):$($b.Port)"
        }
        wslc remove -f $name *> $null
        # socat's 8K default buffer costs ~3x throughput.
        $out = wslc run -d --name $name --network $Network -l "$TargetLabel=$Id" -p $pub `
            $Image -b 262144 $listen $connect 2>&1
        if ($LASTEXITCODE -eq 0) { Log "$target  $pub  -> $name" }
        else { Log "FAILED $target $pub : $($out | Out-String)" }
    }
}

# Map running containers that have no sidecar; drop sidecars whose target is gone.
function Sync-Sidecars {
    $sidecars = @(Get-Sidecars)
    foreach ($c in Invoke-Api GET '/containers/json') {
        if (-not ($sidecars | Where-Object Target -eq $c.Id)) { Add-Sidecars $c.Id }
    }
    foreach ($s in $sidecars) {
        $running = try { (Invoke-Api GET "/containers/$($s.Target)/json").State.Running } catch { $false }
        if (-not $running) { wslc remove -f $s.Name *> $null; Log "removed stale $($s.Name)" }
    }
}

# $Since replays events from before the sync, so a container started between
# Sync-Sidecars and the subscription is not missed.
function Watch-Events([long]$Since) {
    $f = Get-Filter @{ type = @('container'); event = @('start', 'die', 'destroy') }
    $client = [System.Net.Http.HttpClient]::new()
    $client.Timeout = [System.Threading.Timeout]::InfiniteTimeSpan
    try {
        $resp = $client.GetAsync("$Api/events?since=$Since&filters=$f",
            [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
        $reader = [System.IO.StreamReader]::new($resp.Content.ReadAsStreamAsync().GetAwaiter().GetResult())
        while ($null -ne ($line = $reader.ReadLine())) {
            if (-not $line) { continue }
            $e = $line | ConvertFrom-Json
            try {
                if ($e.Action -eq 'start') { Add-Sidecars $e.Actor.ID } else { Remove-Sidecars $e.Actor.ID }
            } catch { Log "event $($e.Action) $($e.Actor.ID): $_" }
        }
    } finally { $client.Dispose() }
}

# ---------------------------------------------------------------- actions

switch ($Action) {
    'start' {
        Initialize-Relay
        $v = Invoke-Api GET '/version'
        Log "Engine API $($v.Version) (API $($v.ApiVersion)) on tcp://127.0.0.1:$Port"

        if (Get-WatcherProcess) {
            Log "watcher already running (pid $((Get-WatcherProcess).Id))"
        } else {
            $exe = (Get-Process -Id $PID).Path
            $p = Start-Process $exe -WindowStyle Hidden -PassThru -ArgumentList @(
                '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', "`"$PSCommandPath`"", 'watch', '-Port', $Port)
            Set-Content $PidFile $p.Id
            Log "watcher started (pid $($p.Id)), log: $LogFile"
        }
        Write-Host ''
        Write-Host "  `$env:DOCKER_HOST = 'tcp://127.0.0.1:$Port'"
        Write-Host "  docker version --format '{{.Server.Version}} (API {{.Server.APIVersion}})'"
    }

    'stop' {
        $w = Get-WatcherProcess
        if ($w) { Stop-Process -Id $w.Id -Force; Log "watcher stopped (pid $($w.Id))" }
        Remove-Item $PidFile -ErrorAction SilentlyContinue
        if (Test-Api) { Remove-Sidecars $null }
        Remove-Relay
        Log 'relay removed'
    }

    'status' {
        $w = Get-WatcherProcess
        if (Test-Api) {
            $v = Invoke-Api GET '/version'
            Write-Host "relay    up    tcp://127.0.0.1:$Port  (engine $($v.Version), API $($v.ApiVersion))"
        } else {
            Write-Host "relay    down"
        }
        Write-Host ("watcher  {0}" -f $(if ($w) { "up    pid $($w.Id)  log $LogFile" } else { 'down' }))
        if (Test-Api) {
            foreach ($s in Get-Sidecars) { Write-Host "sidecar  $($s.Name)" }
        }
    }

    'watch' {
        Log "watcher up (pid $PID)"
        while ($true) {
            try {
                if (-not (Test-Api)) { Log 'Engine API down, re-initialising relay'; Initialize-Relay }
                $since = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() - 1
                Sync-Sidecars
                Watch-Events $since
                Log 'event stream ended'
            } catch { Log "watcher: $_" }
            Start-Sleep -Seconds 5
        }
    }
}
