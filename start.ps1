# opencode2api-dashboard 一键启动
# 启动顺序：网关（opencode2api.exe）→ 仪表盘（dashboard.mjs）
# 已在运行则跳过；全部以隐藏窗口后台运行，日志写文件。
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File start.ps1          # 前台一键启动
#   powershell -ExecutionPolicy Bypass -File start.ps1 -Stop    # 停止全部
#   powershell -ExecutionPolicy Bypass -File start.ps1 -Restart # 重启全部

param(
  [switch]$Stop,
  [switch]$Restart
)

$Root = Split-Path -Parent $MyInvocation.MyCommand.Path
$GatewayExe = Join-Path $Root "opencode2api.exe"
$DashboardScript = Join-Path $Root "dashboard.mjs"
$GatewayLog = Join-Path $Root "opencode2api.log"
$GatewayErr = Join-Path $Root "opencode2api.err.log"
$DashLog = Join-Path $Root "dashboard.log"
$DashErr = Join-Path $Root "dashboard.err.log"
$HealthUrl = "http://127.0.0.1:8080/healthz"
$DashUrl = "http://127.0.0.1:9090"

function IsListening($url) {
  try {
    $r = Invoke-WebRequest -Uri $url -UseBasicParsing -TimeoutSec 3 -ErrorAction Stop
    return $true
  } catch {
    return $false
  }
}

function Stop-All {
  Get-Process -Name opencode2api -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
  Get-Process -Name node -ErrorAction SilentlyContinue | Where-Object { $_.Path -match "node" -and (Get-CimInstance Win32_Process -Filter "ProcessId=$($_.Id)").CommandLine -match "dashboard\.mjs" } | Stop-Process -Force -ErrorAction SilentlyContinue
  Write-Host "[stop] opencode2api / dashboard 已停止"
}

function Start-Gateway {
  if (IsListening $HealthUrl) {
    Write-Host "[gateway] 已在运行 (http://127.0.0.1:8080)"
    return
  }
  Start-Process -FilePath $GatewayExe -WorkingDirectory $Root -WindowStyle Hidden `
    -RedirectStandardOutput $GatewayLog -RedirectStandardError $GatewayErr
  Write-Host "[gateway] 已启动 -> http://127.0.0.1:8080"
}

function Start-Dashboard {
  if (IsListening $DashUrl) {
    Write-Host "[dashboard] 已在运行 (http://127.0.0.1:9090)"
    return
  }
  Start-Process -FilePath "node" -ArgumentList "dashboard.mjs" -WorkingDirectory $Root -WindowStyle Hidden `
    -RedirectStandardOutput $DashLog -RedirectStandardError $DashErr
  Write-Host "[dashboard] 已启动 -> http://127.0.0.1:9090"
}

if ($Stop) { Stop-All; return }
if ($Restart) { Stop-All; Start-Sleep -Seconds 2 }

if (-not (Test-Path $GatewayExe)) {
  Write-Error "未找到 $GatewayExe，请先构建: go build -o opencode2api.exe ."
  exit 1
}
if (-not (Test-Path (Join-Path $Root "config.json"))) {
  Write-Error "未找到 config.json，请先: Copy-Item config.example.json config.json 并编辑"
  exit 1
}

Start-Gateway
Start-Dashboard

Start-Sleep -Seconds 3
if (IsListening $HealthUrl) { Write-Host "[ok] 网关健康检查通过" } else { Write-Host "[warn] 网关尚未就绪，请稍后查看 $GatewayLog" }
Write-Host "`n仪表盘: http://127.0.0.1:9090`n用量API: curl -H `"Authorization: Bearer <key>`" http://127.0.0.1:8080/v1/stats`nPrometheus: http://127.0.0.1:8080/metrics"
