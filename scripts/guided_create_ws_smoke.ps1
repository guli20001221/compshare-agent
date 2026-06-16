param(
    [string]$WsUrl = "ws://127.0.0.1:8080/?Action=CreateCSAgentWS",
    [string]$SessionId = "",
    [string]$Message = "帮我部署 Qwen2.5-32B",
    [string]$ProjectId = "",
    [int]$CompanyId = 1,
    [int]$OrganizationId = 2,
    [string]$GpuType = "",
    [string]$Zone = "",
    [string]$SpecKey = "",
    [switch]$EditImageOnFinal,
    [switch]$ConfirmCreate,
    [switch]$IUnderstandThisCreatesInstance,
    [int]$TimeoutSeconds = 300
)

# Version 1.0 (not Latest): still flags typo'd variables, but lets us read
# optional (omitempty) JSON props — Step.Final, Option.Note, Option.Disabled —
# which are simply absent on non-final cards / available options.
Set-StrictMode -Version 1.0
$ErrorActionPreference = "Stop"

if ($ConfirmCreate -and -not $IUnderstandThisCreatesInstance) {
    throw "Refusing to create a billable instance. Re-run with -IUnderstandThisCreatesInstance."
}

if ([string]::IsNullOrWhiteSpace($SessionId)) {
    $SessionId = "guided-smoke-" + [guid]::NewGuid().ToString("N")
}

if ($WsUrl -notmatch "Action=") {
    $sep = "?"
    if ($WsUrl.Contains("?")) { $sep = "&" }
    $WsUrl = $WsUrl.TrimEnd("/") + $sep + "Action=CreateCSAgentWS"
}

function Send-JsonFrame {
    param(
        [System.Net.WebSockets.ClientWebSocket]$Socket,
        [hashtable]$Frame,
        [System.Threading.CancellationToken]$Token
    )
    $json = ($Frame | ConvertTo-Json -Depth 20 -Compress)
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
    $segment = [System.ArraySegment[byte]]::new($bytes)
    [void]$Socket.SendAsync($segment, [System.Net.WebSockets.WebSocketMessageType]::Text, $true, $Token).GetAwaiter().GetResult()
}

function Receive-JsonFrame {
    param(
        [System.Net.WebSockets.ClientWebSocket]$Socket,
        [System.Threading.CancellationToken]$Token
    )
    $buffer = New-Object byte[] 8192
    $ms = [System.IO.MemoryStream]::new()
    do {
        $segment = [System.ArraySegment[byte]]::new($buffer)
        $result = $Socket.ReceiveAsync($segment, $Token).GetAwaiter().GetResult()
        if ($result.MessageType -eq [System.Net.WebSockets.WebSocketMessageType]::Close) {
            throw "WebSocket closed before terminal frame."
        }
        if ($result.Count -gt 0) {
            $ms.Write($buffer, 0, $result.Count)
        }
    } while (-not $result.EndOfMessage)

    $text = [System.Text.Encoding]::UTF8.GetString($ms.ToArray())
    return $text | ConvertFrom-Json
}

function Find-Field {
    param($Form, [string]$Key)
    if ($null -eq $Form -or $null -eq $Form.Fields) { return $null }
    foreach ($field in $Form.Fields) {
        if ($field.Key -eq $Key) { return $field }
    }
    return $null
}

function Pick-OptionValue {
    param($Field, [string]$Preferred)
    if ($null -eq $Field -or $null -eq $Field.Options) { return "" }
    if (-not [string]::IsNullOrWhiteSpace($Preferred)) {
        foreach ($option in $Field.Options) {
            if ($option.Value -eq $Preferred -and -not [bool]$option.Disabled) {
                return [string]$option.Value
            }
        }
        throw "Preferred value '$Preferred' is not available for field '$($Field.Key)'."
    }
    foreach ($option in $Field.Options) {
        if (-not [bool]$option.Disabled) {
            return [string]$option.Value
        }
    }
    return ""
}

function Send-Confirm {
    param(
        [System.Net.WebSockets.ClientWebSocket]$Socket,
        [string]$ConfirmationId,
        [bool]$Confirmed,
        [hashtable]$Overrides,
        [System.Threading.CancellationToken]$Token
    )
    $frame = @{
        Action = "ConfirmCSAgentAction"
        SessionId = $SessionId
        ConfirmationId = $ConfirmationId
        Confirmed = $Confirmed
    }
    if ($Confirmed -and $Overrides.Count -gt 0) {
        $frame["Overrides"] = $Overrides
    }
    Send-JsonFrame -Socket $Socket -Frame $frame -Token $Token
}

$cts = [System.Threading.CancellationTokenSource]::new([TimeSpan]::FromSeconds($TimeoutSeconds))
$socket = [System.Net.WebSockets.ClientWebSocket]::new()
$socket.Options.SetRequestHeader("X-Company-Id", [string]$CompanyId)
$socket.Options.SetRequestHeader("X-Organization-Id", [string]$OrganizationId)
$socket.Options.SetRequestHeader("X-Request-Id", "guided-smoke-" + [guid]::NewGuid().ToString("N"))

try {
    Write-Host "Connecting $WsUrl"
    $socket.ConnectAsync([Uri]$WsUrl, $cts.Token).GetAwaiter().GetResult()

    $send = @{
        Action = "SendCSAgentChat"
        SessionId = $SessionId
        Message = $Message
        Features = @("confirm_form_v1", "guided_create_v1")
    }
    if (-not [string]::IsNullOrWhiteSpace($ProjectId)) {
        $send["ProjectId"] = $ProjectId
    }
    Send-JsonFrame -Socket $socket -Frame $send -Token $cts.Token
    Write-Host "Sent request: $Message"

    $seenSteps = @{}
    $finalCount = 0
    $editedFinalImage = $false
    $requestedFinalEdit = $false

    while ($true) {
        $frame = Receive-JsonFrame -Socket $socket -Token $cts.Token
        $event = [string]$frame.event
        if ([string]::IsNullOrWhiteSpace($event)) { $event = "(no event)" }

        if ($event -eq "confirmation") {
            $form = $frame.Form
            $step = $form.Step
            if ($null -eq $step) {
                throw "Expected guided form, got legacy confirmation."
            }
            $idx = [int]$step.Index
            $seenSteps[[string]$idx] = $true
            Write-Host ("Confirmation step {0}/{1}: {2}" -f $step.Index, $step.Total, $step.Title)

            if ($step.Description) {
                Write-Host ("  guidance: {0}" -f $step.Description)
            }
            foreach ($f in $form.Fields) {
                $optDump = (($f.Options | ForEach-Object { $_.Value }) -join ", ")
                Write-Host ("  field {0} (default={1}) options=[{2}]" -f $f.Key, $f.Value, $optDump)
                foreach ($o in $f.Options) {
                    if ($o.Note) { Write-Host ("    - {0}: {1}" -f $o.Value, $o.Note) }
                }
            }

            if ([bool]$step.Final) {
                # Final card: optional one-time image edit (revalidation path), then confirm/decline.
                if ($EditImageOnFinal -and -not $editedFinalImage) {
                    $imageField = Find-Field -Form $form -Key "ImageId"
                    $candidate = ""
                    if ($null -ne $imageField -and $null -ne $imageField.Options) {
                        foreach ($option in $imageField.Options) {
                            if (-not [bool]$option.Disabled -and [string]$option.Value -ne [string]$imageField.Value) {
                                $candidate = [string]$option.Value
                                break
                            }
                        }
                    }
                    if ($candidate -ne "") {
                        $requestedFinalEdit = $true
                        $editedFinalImage = $true
                        $finalCount++
                        Send-Confirm -Socket $socket -ConfirmationId $frame.ConfirmationId -Confirmed $true -Overrides @{ ImageId = $candidate } -Token $cts.Token
                        Write-Host "Edited final image; waiting for refreshed final confirmation."
                        continue
                    }
                    Write-Host "No alternate image was available; skipping final image edit."
                }
                $finalCount++
                Send-Confirm -Socket $socket -ConfirmationId $frame.ConfirmationId -Confirmed ([bool]$ConfirmCreate) -Overrides @{} -Token $cts.Token
                if ($ConfirmCreate) {
                    Write-Host "Final confirmation accepted; this may create a billable instance."
                } else {
                    Write-Host "Final confirmation declined intentionally; no instance should be created."
                }
                continue
            }

            # Intermediate card (GPU / Zone / 卡数 / CPU·内存): accept the preferred value
            # for a known key, else the card's own default. Step-count-agnostic.
            $overrides = @{}
            foreach ($f in $form.Fields) {
                if (-not [bool]$f.Editable) { continue }
                $preferred = ""
                switch ($f.Key) {
                    "GpuType"   { $preferred = $GpuType }
                    "Zone"      { $preferred = $Zone }
                    "CpuMemory" { $preferred = $SpecKey }
                }
                $value = Pick-OptionValue -Field $f -Preferred $preferred
                if ($value -ne "" -and $value -ne [string]$f.Value) {
                    $overrides[$f.Key] = $value
                }
            }
            Send-Confirm -Socket $socket -ConfirmationId $frame.ConfirmationId -Confirmed $true -Overrides $overrides -Token $cts.Token
            continue
        }

        if ($event -eq "token") {
            if ($frame.Text) { Write-Host -NoNewline ([string]$frame.Text) }
            continue
        }

        if ($event -eq "error") {
            throw ("Server error: " + ($frame | ConvertTo-Json -Depth 10 -Compress))
        }

        if ($event -eq "done") {
            Write-Host ""
            break
        }
    }

    foreach ($required in @("1", "2", "3", "4", "5")) {
        if (-not $seenSteps.ContainsKey($required)) {
            throw "Guided step $required was not observed."
        }
    }
    if ($requestedFinalEdit -and $finalCount -lt 2) {
        throw "Final image edit did not produce a refreshed final confirmation."
    }
    Write-Host "Guided create WS smoke passed. SessionId=$SessionId"
} finally {
    if ($socket.State -eq [System.Net.WebSockets.WebSocketState]::Open) {
        $socket.CloseAsync([System.Net.WebSockets.WebSocketCloseStatus]::NormalClosure, "done", [System.Threading.CancellationToken]::None).GetAwaiter().GetResult()
    }
    $socket.Dispose()
    $cts.Dispose()
}
