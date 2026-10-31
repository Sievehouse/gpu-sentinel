# End-to-end demo on synthetic data, for Windows PowerShell 5.1 and PowerShell 7
# (Windows, Linux or macOS). It runs the same steps as scripts/demo.sh.
# Needs Go 1.22+. Run it from anywhere:
#
#   powershell -ExecutionPolicy Bypass -File scripts\demo.ps1
#
# Files go to a new temporary directory, or to $env:WORK if it is set.
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2

$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root
if ($env:WORK) { $Work = $env:WORK } else {
  $Work = Join-Path ([IO.Path]::GetTempPath()) ("capledger-demo-" + [guid]::NewGuid().ToString('N').Substring(0, 8))
}
New-Item -ItemType Directory -Force -Path $Work | Out-Null

function Step([string]$text) { Write-Host ""; Write-Host "==> $text" -ForegroundColor Cyan }
function W([string[]]$parts) { Join-Path $Work ($parts -join [IO.Path]::DirectorySeparatorChar) }
# Write text as UTF-8 without a byte-order mark (Set-Content and Out-File add
# one in Windows PowerShell, which JSON readers reject).
function Save([string]$path, [string]$text) { [IO.File]::WriteAllText($path, $text, (New-Object Text.UTF8Encoding $false)) }

$Exe = 'capledger'
if ($env:OS -eq 'Windows_NT') { $Exe = 'capledger.exe' }
$CL = W $Exe

# Run capledger, print its output and stop the demo if it fails.
function CL {
  & $CL @args
  if ($LASTEXITCODE -ne 0) { throw "capledger $($args -join ' ') failed with exit code $LASTEXITCODE" }
}
# Run capledger and return its standard output as one string.
function CLOut {
  $out = & $CL @args
  if ($LASTEXITCODE -ne 0) { throw "capledger $($args -join ' ') failed with exit code $LASTEXITCODE" }
  ($out | Out-String)
}

Step "Build capledger"
& go build -o $CL ./cmd/capledger
if ($LASTEXITCODE -ne 0) { throw "go build failed" }
CL version

Step "Replay a sample kernel log through the agent; the node gets its own signing key"
$Log = W 'evidence-gpu-003.jsonl'
$Keys = W 'keys', 'gpu-003'
CL agent --node gpu-003 --source examples/samples/kern.log --once --log $Log --key-dir $Keys

Step "Record the repair that closes the incident (normally done by an integration)"
CL event emit --log $Log --key-dir $Keys --node gpu-003 --type up --ts 2026-10-03T06:40:00Z --cause node-reseated --source provider-health-check

Step "Trust the key the agent announced, for node gpu-003 only"
$KR = W 'keyring.json'
CL keyring import --keyring $KR --log $Log --owner "Example Neocloud" --role provider-agent
CL keyring list --keyring $KR

Step "Verify the log against the keyring (hash chain, signatures, node bindings)"
CL evidence verify --log $Log --keyring $KR --format text

Step "Export the signed events for the SLA engine"
$events = CLOut events export --log $Log --keyring $KR
Write-Host $events.TrimEnd()
Save (W 'agent-events.jsonl') $events

Step "AMD: replay amdgpu kernel messages; the uncorrectable RAS error becomes a down event"
CL agent --vendor amd --node mi355-02 --source examples/samples/kern-amd.log --once --log (W 'evidence-mi355-02.jsonl') --key-dir (W 'keys', 'mi355-02')
CL events export --log (W 'evidence-mi355-02.jsonl') --pub (W 'keys', 'mi355-02', 'capledger.pub.pem')

Step "Redaction: process names and ids that could identify a customer's job never reach the log"
(CLOut xid parse --file examples/samples/kern.log --redact) -split "`n" | Where-Object { $_ -match '"code":94' }

Step "Kubernetes: two saved views of the cluster (an NVSentinel fault, a lost GPU, then the repair) become signed events"
foreach ($view in @(@('2026-10-03T0415', '2026-10-03T04:15:00Z'), @('2026-10-03T0645', '2026-10-03T06:45:00Z'))) {
  CL watch kubernetes --from "examples/kubernetes/nodes-$($view[0]).json" --at $view[1] --once `
    '--down-condition=*' --expected-gpus 8 --log (W 'k8s-watcher.jsonl') --key-dir (W 'keys', 'k8s-watcher')
}

Step "Heartbeats: a node that went silent for 27 minutes shows up as unmonitored time"
$HB = W 'evidence-gpu-001.jsonl'
foreach ($t in @('00:00', '00:01', '00:02', '00:30', '00:31')) {
  Save (W 'hb.json') ('{"node":"gpu-001","ts":"2026-10-01T' + $t + ':00Z","intervalSeconds":60,"role":"agent"}')
  CLOut evidence append --log $HB --key-dir (W 'keys', 'gpu-001') --kind heartbeat --payload (W 'hb.json') | Out-Null
}
CL evidence gaps --log $HB --pub (W 'keys', 'gpu-001', 'capledger.pub.pem') --max-silence 5m --since 2026-10-01T00:00:00Z --until 2026-10-01T00:31:00Z

Step "Compute the October statement for the 16-node example contract (YAML)"
$Statement = W 'statement.json'
CL sla compute --contract examples/contracts/hgx-16-node.yaml --events examples/events/hgx-16-node-2026-10.jsonl --period 2026-10 --out $Statement --format text

Step "Replay the statement independently (what the customer or an insurer would run)"
CL sla verify --statement $Statement --contract examples/contracts/hgx-16-node.yaml --events examples/events/hgx-16-node-2026-10.jsonl

Step "The same month with three unmonitored stretches, under a contract that counts them as downtime"
(CLOut sla compute --contract examples/contracts/hgx-16-node-strict.yaml --events examples/events/hgx-16-node-2026-10.jsonl `
  --events examples/events/hgx-16-node-2026-10-gaps.jsonl --period 2026-10 --format text) -split "`n" | Select-Object -First 7

Step "Compare the provider's record with the customer's own probe"
CL sla corroborate --contract examples/contracts/hgx-16-node.yaml --period 2026-10 `
  --events-a examples/events/hgx-16-node-2026-10.jsonl --events-b examples/events/hgx-16-node-2026-10-tenant.jsonl

Step "Seal the statement into the evidence log and verify again"
CL evidence append --log $Log --key-dir $Keys --kind statement --payload $Statement
CL evidence verify --log $Log --keyring $KR --format text

Step "Freeze the statement and its inputs in a signed bundle, then check and unpack it"
$Bundle = W 'statement-2026-10.tar.gz'
CL bundle create --out $Bundle --type statement --subject example-neocloud--example-lab--2026 --key-dir (W 'keys', 'auditor') `
  --file $Statement --file contract.yaml=examples/contracts/hgx-16-node.yaml --file events.jsonl=examples/events/hgx-16-node-2026-10.jsonl --file $Log
CL bundle verify --bundle $Bundle --pub (W 'keys', 'auditor', 'capledger.pub.pem') --extract (W 'unpacked')

Step "Rack-scale example: three NVL72 trays overlap, so the whole rack counts as down"
(CLOut sla compute --contract examples/contracts/nvl72-2-racks.json --events examples/events/nvl72-2026-10.jsonl --period 2026-10 --format text) -split "`n" | Select-Object -First 8

Step "Version 2 of the contract says whose clock counts: from the provider's first fault to the tenant's recovery"
CL contract check --contract examples/contracts/hgx-16-node-reconciled.yaml
$lines = (CLOut sla compute --contract examples/contracts/hgx-16-node-reconciled.yaml --events examples/events/hgx-16-node-2026-10-both.jsonl --period 2026-10 --format text) -split "`n"
$lines[0..5] + $lines[7..11]

Step "Each party signs the October statement; it counts as agreed only when both accept"
foreach ($p in @(@('provider', 'Example Neocloud'), @('customer', 'Example Lab'))) {
  $role = $p[0]; $owner = $p[1]
  CLOut keygen --out (W 'keys', "$role-signer") | Out-Null
  CL keyring add --keyring $KR --pub (W 'keys', "$role-signer", 'capledger.pub.pem') --owner $owner --role $role
  CL sla sign --statement $Statement --party $role --key-dir (W 'keys', "$role-signer")
}
CL sla verify --statement $Statement --keyring $KR --require 'provider,customer' `
  --signature (W 'statement.provider.sig.json') --signature (W 'statement.customer.sig.json')

Step "Run a ledger for the contract: both parties push, a witness checkpoints, a financier reads the feed"
$Probe = W 'probe-gpu-003.jsonl'
CLOut event emit --log $Probe --key-dir (W 'keys', 'probe-gpu-003') --role tenant-probe --node gpu-003 --type down `
  --ts 2026-10-03T04:13:00Z --cause tenant:gpu-missing:7/8 --source tenant-probe | Out-Null
CL keyring import --keyring $KR --log $Probe --owner "Example Lab" --role tenant-probe
CLOut keygen --out (W 'keys', 'insurer-signer') | Out-Null
CL keyring add --keyring $KR --pub (W 'keys', 'insurer-signer', 'capledger.pub.pem') --owner "Example Insurer" --role financier
# The configuration is written by hand rather than with ConvertTo-Json, whose
# output for nested arrays differs between PowerShell versions.
$parties = @()
foreach ($p in @(@('example-neocloud', 'provider', 'provider-agent'), @('example-lab', 'customer', 'tenant-probe'), @('example-insurer', 'financier', ''))) {
  $out = CLOut ledger token --out (W "$($p[0]).token")
  if ($out -notmatch 'tokenSha256\s+([0-9a-f]{64})') { throw "no token hash in: $out" }
  $roles = ''
  if ($p[2]) { $roles = ', "writeRoles": ["' + $p[2] + '"]' }
  $parties += '{"name": "' + $p[0] + '", "role": "' + $p[1] + '", "tokenSha256": "' + $Matches[1] + '"' + $roles + '}'
}
Save (W 'ledger.json') ('{"apiVersion": "capacity-ledger/v1alpha1", "kind": "LedgerConfig", "contract": "example-neocloud--example-lab--2026",' + "`n" +
  ' "parties": [' + ($parties -join ', ') + ']}' + "`n")

$LedgerLog = W 'ledger.log'
$ledgerArgs = @('ledger', 'serve', '--config', (W 'ledger.json'), '--keyring', $KR, '--data', (W 'ledger-data'),
  '--key-dir', (W 'keys', 'witness'), '--listen', '127.0.0.1:0') | ForEach-Object { if ($_ -match '\s') { '"' + $_ + '"' } else { $_ } }
$ledger = Start-Process -FilePath $CL -ArgumentList $ledgerArgs -PassThru -NoNewWindow -RedirectStandardError $LedgerLog
try {
  $Url = $null
  for ($i = 0; $i -lt 100 -and -not $Url; $i++) {
    Start-Sleep -Milliseconds 100
    if (Test-Path $LedgerLog) {
      $m = Select-String -Path $LedgerLog -Pattern 'http://127\.0\.0\.1:[0-9]+' | Select-Object -First 1
      if ($m) { $Url = $m.Matches[0].Value }
    }
  }
  if (-not $Url) { Get-Content $LedgerLog; throw "the ledger did not start" }
  function As([string]$who) { CL ledger @args --server $Url --token-file (W "$who.token") }
  As example-neocloud push --log $Log
  As example-lab push --log $Probe
  As example-lab pull --all --out-dir (W 'pulled')
  As example-lab checkpoint --create --out (W 'checkpoint.json')
  As example-lab status --witness-pub (W 'witness.pub.pem')
  CL keyring add --keyring $KR --pub (W 'witness.pub.pem') --owner "Ledger operator" --role witness
  CL evidence verify --keyring $KR --checkpoint (W 'checkpoint.json') --format text `
    --log (W 'pulled', 'evidence-gpu-003.jsonl') --log (W 'pulled', 'probe-gpu-003.jsonl')
  As example-neocloud publish --statement $Statement --signature (W 'statement.provider.sig.json')
  As example-lab publish --signature (W 'statement.customer.sig.json')
  As example-neocloud consent --financier example-insurer --party provider --key-dir (W 'keys', 'provider-signer')
  As example-lab consent --financier example-insurer --party customer --key-dir (W 'keys', 'customer-signer')
  As example-insurer feed --out-dir (W 'feed')
} finally {
  if (-not $ledger.HasExited) { Stop-Process -Id $ledger.Id -Force -ErrorAction SilentlyContinue }
}

Write-Host ""
Write-Host "Done. Files are in $Work" -ForegroundColor Green
