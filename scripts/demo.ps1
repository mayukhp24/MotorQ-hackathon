<#
.SYNOPSIS
  FleetPulse demo helper for Windows. Needs only Docker Desktop: the Python
  tools run in the stack's "demo-tools" container.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File scripts\demo.ps1 check

  check           readiness check before recording (run 5-10 minutes before)
  samples         F-01: one payload in each OEM format (Aurora, Pinnacle, Stellar)
  corrupt         F-02: a Pinnacle batch with one bad VIN -> 1 accepted, 1 to the DLQ
  overheat        F-03: acceptance tests; an overheating vehicle raises a critical alert
  kill-processor  F-15: kill one stream-processor replica, restart it after 40 s
#>
param(
  [Parameter(Position = 0)]
  [ValidateSet("check", "samples", "corrupt", "overheat", "kill-processor")]
  [string]$Command = "check"
)

# Native commands report failure through $LASTEXITCODE; keep going and check it.
$ErrorActionPreference = "Continue"
Set-Location (Split-Path -Parent $PSScriptRoot)
$Api = "http://localhost:8000/api/v1"
$Password = "FleetPulse!2026"

function Say([string]$Text, [string]$Color = "Gray") { Write-Host $Text -ForegroundColor $Color }
function Ok([string]$Text) { Say "  [ok]   $Text" "Green" }
function Bad([string]$Text) { Say "  [fix]  $Text" "Red"; $script:Problems++ }
function Note([string]$Text) { Say "  [note] $Text" "Yellow" }

function Get-Token([string]$Email) {
  $body = @{ username = $Email; password = $Password; grant_type = "password" }
  (Invoke-RestMethod -Method Post -Uri "$Api/auth/token" -Body $body -ContentType "application/x-www-form-urlencoded" -TimeoutSec 15).access_token
}

function Start-Tools {
  docker compose --profile demo up -d demo-tools 2>&1 | Out-Null
  for ($i = 0; $i -lt 60; $i++) {
    docker compose exec -T demo-tools python -c "import behave, requests" 2>&1 | Out-Null
    if ($LASTEXITCODE -eq 0) { return $true }
    Start-Sleep -Seconds 2
  }
  return $false
}

function Invoke-Tools([string[]]$ToolArgs, [string]$WorkDir = "/w") {
  if (-not (Start-Tools)) { Say "demo-tools is not ready; see: docker compose logs demo-tools" "Red"; exit 1 }
  # In a real console, give the command a terminal so its output is coloured.
  $tty = if ([Console]::IsInputRedirected -or [Console]::IsOutputRedirected) { "-T" } else { "-t" }
  docker compose exec $tty -w $WorkDir demo-tools @ToolArgs
}

switch ($Command) {
  "check" {
    $script:Problems = 0
    Say "`nFleetPulse demo readiness" "Cyan"

    $required = "kafka", "postgres", "redis", "clickhouse", "mosquitto", "ingest-gateway", "stream-processor",
                "sink-writer", "simulator", "fleet-api", "web", "analytics", "prometheus", "grafana"
    $running = @(docker compose ps --services --status running 2>$null)
    $missing = @($required | Where-Object { $running -notcontains $_ })
    if ($missing.Count -eq 0) { Ok "all $($required.Count) services running" }
    else { Bad "not running: $($missing -join ', ')  ->  docker compose up -d   (then: docker compose logs <name>)" }

    try {
      $token = Get-Token "maint@acme.demo"
      $summary = Invoke-RestMethod -Uri "$Api/fleet/summary" -Headers @{ Authorization = "Bearer $token" } -TimeoutSec 15
      $eps = [math]::Round([double]$summary.live.eps)
      $online = [int]$summary.live.online
      if ($eps -gt 0 -and $online -gt 0) { Ok "live data flowing: $eps events/s, $online Acme vehicles online" }
      else { Bad "no live data (events/s $eps, online $online)  ->  docker compose restart mosquitto ingest-gateway simulator" }
      $highRisk = [int]$summary.risk.high_risk
      if ($highRisk -gt 0) { Ok "predictions ready: $highRisk high-risk vehicles, scored on $($summary.risk.scored_on)" }
      else { Bad "no risk scores yet  ->  docker compose exec analytics python -m analytics.run all" }
    } catch {
      Bad "API not answering at $Api ($($_.Exception.Message))  ->  docker compose up -d fleet-api"
      $token = $null
    }

    $mode = docker compose logs fleet-api 2>&1 | Select-String "copilot mode: (llm \([a-z]+\)|[a-z]+)" | Select-Object -Last 1
    if ($mode) { $mode = $mode.Matches[0].Groups[1].Value }
    if ($mode -like "llm*") { Ok "copilot mode: $mode" }
    else { Note "copilot mode: $mode (no LLM key set: answers come from the offline planner, which is fine on camera)" }

    if ($token) {
      try {
        $ask = @{ message = "How much are we losing to idling, and where?" } | ConvertTo-Json
        $turn = Invoke-RestMethod -Method Post -Uri "$Api/copilot/chat" -Headers @{ Authorization = "Bearer $token" } `
          -ContentType "application/json" -Body $ask -TimeoutSec 90
        $secs = [math]::Round($turn.latency_ms / 1000, 1)
        if ($turn.mode -eq "llm") {
          Ok "copilot answered with the LLM in $secs s"
          Note "that used one LLM request; on a free tier wait a minute before recording the copilot segment"
        }
        else { Note "copilot answered in '$($turn.mode)' mode in $secs s; $($turn.warnings -join '; ')" }
      } catch { Bad "copilot request failed: $($_.Exception.Message)" }
    }

    try { Invoke-WebRequest -UseBasicParsing -Uri "http://localhost:3000/" -TimeoutSec 10 | Out-Null; Ok "web app: http://localhost:3000" }
    catch { Bad "web app not answering  ->  docker compose up -d web" }
    try { Invoke-WebRequest -UseBasicParsing -Uri "http://localhost:3001/api/health" -TimeoutSec 10 | Out-Null; Ok "Grafana: http://localhost:3001" }
    catch { Bad "Grafana not answering  ->  docker compose up -d grafana" }

    if (Start-Tools) { Ok "demo tools ready (samples / corrupt / overheat)" }
    else { Bad "demo tools not ready  ->  docker compose logs demo-tools" }

    if ($script:Problems -eq 0) { Say "`nREADY TO RECORD" "Green" }
    else { Say "`n$($script:Problems) thing(s) to fix before recording" "Red" }
  }

  "samples" { Invoke-Tools @("python", "scripts/send-sample.py") }

  "corrupt" { Invoke-Tools @("python", "scripts/send-sample.py", "pinnacle", "--corrupt") }

  "overheat" { Invoke-Tools @("behave", "--format", "pretty", "features/realtime_alerts.feature") "/w/tests/bdd" }

  "kill-processor" {
    $ids = @(docker compose ps -q stream-processor)
    if ($ids.Count -lt 2) { Say "need 2 stream-processor replicas running (found $($ids.Count))" "Red"; exit 1 }
    $victim = (docker inspect -f "{{.Name}}" $ids[0]).TrimStart("/")
    Say "docker kill $victim" "Cyan"
    docker kill $victim | Out-Null
    Say "Killed. Its partitions move to the surviving replica in ~11 s; watch lag in Grafana." "Yellow"
    for ($s = 40; $s -gt 0; $s -= 10) { Say "  restarting it in $s s..."; Start-Sleep -Seconds 10 }
    docker start $victim | Out-Null
    Say "Restarted $victim; partitions rebalance across both replicas again." "Green"
  }
}
