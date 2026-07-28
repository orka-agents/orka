[CmdletBinding()]
param(
    [ValidateSet('malicious', 'benign')]
    [string] $Fixture,

    [ValidateSet('codex', 'claude', 'pi')]
    [string[]] $Engine,

    [Alias('h')]
    [switch] $Help
)

$ErrorActionPreference = 'Stop'

$Harness = Join-Path $PSScriptRoot 'test-review-harness.py'
$ScriptRoot = (Resolve-Path -LiteralPath $PSScriptRoot).Path
$CurrentRoot = (Get-Location).ProviderPath

if ([IO.Path]::DirectorySeparatorChar -eq '\' -and -not ('AutoreviewPath' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Text;
using Microsoft.Win32.SafeHandles;

public static class AutoreviewPath {
    private const uint FileShareRead = 0x00000001;
    private const uint FileShareWrite = 0x00000002;
    private const uint FileShareDelete = 0x00000004;
    private const uint OpenExisting = 3;
    private const uint FileFlagBackupSemantics = 0x02000000;
    private const uint FileFlagOpenReparsePoint = 0x00200000;
    private const uint FsctlGetReparsePoint = 0x000900A8;
    private const uint IoReparseTagAppExecLink = 0x8000001B;

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern SafeFileHandle CreateFile(
        string fileName,
        uint desiredAccess,
        uint shareMode,
        IntPtr securityAttributes,
        uint creationDisposition,
        uint flagsAndAttributes,
        IntPtr templateFile
    );

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern uint GetFinalPathNameByHandle(
        SafeFileHandle file,
        StringBuilder path,
        uint pathLength,
        uint flags
    );

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool DeviceIoControl(
        SafeFileHandle device,
        uint controlCode,
        IntPtr inputBuffer,
        uint inputBufferSize,
        byte[] outputBuffer,
        uint outputBufferSize,
        out uint bytesReturned,
        IntPtr overlapped
    );

    public static string GetFinalPath(string path) {
        using (SafeFileHandle handle = CreateFile(
            path,
            0,
            FileShareRead | FileShareWrite | FileShareDelete,
            IntPtr.Zero,
            OpenExisting,
            FileFlagBackupSemantics,
            IntPtr.Zero
        )) {
            if (handle.IsInvalid) {
                throw new Win32Exception(Marshal.GetLastWin32Error());
            }
            StringBuilder buffer = new StringBuilder(512);
            uint length = GetFinalPathNameByHandle(
                handle,
                buffer,
                (uint)buffer.Capacity,
                0
            );
            if (length == 0) {
                throw new Win32Exception(Marshal.GetLastWin32Error());
            }
            if (length >= buffer.Capacity) {
                buffer.Capacity = (int)length + 1;
                length = GetFinalPathNameByHandle(
                    handle,
                    buffer,
                    (uint)buffer.Capacity,
                    0
                );
                if (length == 0 || length >= buffer.Capacity) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
            }
            return buffer.ToString();
        }
    }

    public static bool IsAppExecutionAlias(string path) {
        using (SafeFileHandle handle = CreateFile(
            path,
            0,
            FileShareRead | FileShareWrite | FileShareDelete,
            IntPtr.Zero,
            OpenExisting,
            FileFlagBackupSemantics | FileFlagOpenReparsePoint,
            IntPtr.Zero
        )) {
            if (handle.IsInvalid) {
                return false;
            }
            byte[] buffer = new byte[16 * 1024];
            uint bytesReturned;
            if (!DeviceIoControl(
                handle,
                FsctlGetReparsePoint,
                IntPtr.Zero,
                0,
                buffer,
                (uint)buffer.Length,
                out bytesReturned,
                IntPtr.Zero
            ) || bytesReturned < 4) {
                return false;
            }
            return BitConverter.ToUInt32(buffer, 0) == IoReparseTagAppExecLink;
        }
    }
}
'@
}

function Get-FilesystemIdentityPath([string] $Path) {
    try {
        $FullPath = [IO.Path]::GetFullPath($Path)
        if ([IO.Path]::DirectorySeparatorChar -eq '\') {
            if ($FullPath -match '^[\\/]{2}[?.][\\/]') { return $null }
            try {
                return [AutoreviewPath]::GetFinalPath($FullPath)
            }
            catch {
                if ([AutoreviewPath]::IsAppExecutionAlias($FullPath)) {
                    return $FullPath
                }
                throw
            }
        }
        return $FullPath
    }
    catch {
        return $null
    }
}
function Get-CheckoutRoot([string] $Start) {
    $Current = [IO.Path]::GetFullPath($Start)
    while ($true) {
        if (Test-Path -LiteralPath (Join-Path $Current '.git')) {
            return $Current.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
        }
        $Parent = [IO.Directory]::GetParent($Current)
        if ($null -eq $Parent) { return $null }
        $Current = $Parent.FullName
    }
}

function Split-PathComponents([string] $Path) {
    $SeparatorText = [IO.Path]::DirectorySeparatorChar.ToString()
    if ([IO.Path]::AltDirectorySeparatorChar -ne [IO.Path]::DirectorySeparatorChar) {
        $SeparatorText += [IO.Path]::AltDirectorySeparatorChar
    }
    return $Path.Split(
        $SeparatorText.ToCharArray(),
        [StringSplitOptions]::RemoveEmptyEntries
    )
}

function Get-LinkTargetText([IO.FileSystemInfo] $Item) {
    $Targets = @($Item.Target)
    if ($Targets.Count -ne 1 -or [string]::IsNullOrEmpty([string] $Targets[0])) {
        return $null
    }
    return [string] $Targets[0]
}

function Resolve-CanonicalPath([string] $Path) {
    try {
        $Initial = [IO.Path]::GetFullPath($Path)
        if (
            [IO.Path]::DirectorySeparatorChar -eq '\' -and
            $Initial -match '^[\\/]{2}[?.][\\/]'
        ) {
            return $null
        }
        $Root = [IO.Path]::GetPathRoot($Initial)
        if ([string]::IsNullOrEmpty($Root)) { return $null }
        $Current = $Root
        $Traversed = [Collections.Generic.List[string]]::new()
        [void] $Traversed.Add($Root)
        $RootIdentity = Get-FilesystemIdentityPath $Root
        if ($null -eq $RootIdentity) { return $null }
        [void] $Traversed.Add($RootIdentity)
        $Pending = [Collections.Generic.List[string]]::new()
        foreach ($Segment in (Split-PathComponents $Initial.Substring($Root.Length))) {
            [void] $Pending.Add($Segment)
        }
        $LinkHops = 0
        while ($Pending.Count -gt 0) {
            $Segment = $Pending[0]
            $Pending.RemoveAt(0)
            if ($Segment -eq '.') { continue }
            if ($Segment -eq '..') {
                $Parent = [IO.Directory]::GetParent($Current)
                if ($null -ne $Parent) { $Current = $Parent.FullName }
                [void] $Traversed.Add($Current)
                continue
            }
            $Candidate = Join-Path $Current $Segment
            $Item = Get-Item -LiteralPath $Candidate -Force -ErrorAction Stop
            $ItemPath = [IO.Path]::GetFullPath($Item.FullName)
            [void] $Traversed.Add($ItemPath)
            $IdentityPath = Get-FilesystemIdentityPath $ItemPath
            if ($null -eq $IdentityPath) { return $null }
            [void] $Traversed.Add($IdentityPath)
            if ($null -ne $Item.LinkType) {
                $LinkHops++
                if ($LinkHops -gt 128) { return $null }
                $TargetText = Get-LinkTargetText $Item
                if ($null -eq $TargetText) { return $null }
                if (
                    [IO.Path]::DirectorySeparatorChar -eq '\' -and
                    $TargetText -match '^[\\/]{2}[?.][\\/]'
                ) {
                    return $null
                }
                if ([IO.Path]::IsPathRooted($TargetText)) {
                    if (
                        [IO.Path]::DirectorySeparatorChar -eq '\' -and
                        $TargetText -notmatch '^(?:[A-Za-z]:[\\/]|[\\/]{2}[^\\/]+[\\/]+[^\\/]+)'
                    ) {
                        return $null
                    }
                    $TargetRoot = [IO.Path]::GetPathRoot($TargetText)
                    if ([string]::IsNullOrEmpty($TargetRoot)) { return $null }
                    $Current = $TargetRoot
                    [void] $Traversed.Add($TargetRoot)
                    $TargetRootIdentity = Get-FilesystemIdentityPath $TargetRoot
                    if ($null -eq $TargetRootIdentity) { return $null }
                    [void] $Traversed.Add($TargetRootIdentity)
                    $TargetText = $TargetText.Substring($TargetRoot.Length)
                }
                $TargetSegments = @(Split-PathComponents $TargetText)
                for ($Index = $TargetSegments.Count - 1; $Index -ge 0; $Index--) {
                    $Pending.Insert(0, $TargetSegments[$Index])
                }
                continue
            }
            $Current = $ItemPath
        }
        $IdentityPath = Get-FilesystemIdentityPath $Current
        if ($null -eq $IdentityPath) { return $null }
        return [PSCustomObject]@{
            Path = $IdentityPath
            Traversed = $Traversed.ToArray()
        }
    }
    catch {
        return $null
    }
}
$RawCheckoutRoots = @(
    (Get-CheckoutRoot $CurrentRoot),
    (Get-CheckoutRoot $ScriptRoot)
) | Where-Object { $null -ne $_ } | Select-Object -Unique

$CheckoutRoots = @(
    foreach ($Root in $RawCheckoutRoots) {
        $LexicalRoot = [IO.Path]::GetFullPath($Root).TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
        $LexicalRoot
        $CanonicalRoot = Resolve-CanonicalPath $LexicalRoot
        if ($null -eq $CanonicalRoot) {
            throw "unable to canonicalize reviewed checkout root: $LexicalRoot"
        }
        $CanonicalRoot.Path.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar)
    }
) | Where-Object { -not [string]::IsNullOrEmpty($_) } | Select-Object -Unique

function Get-ExternalApplicationPath([string] $Path) {
    try {
        $Item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
        $Lexical = [IO.Path]::GetFullPath($Item.FullName)
        $Resolution = Resolve-CanonicalPath $Lexical
        if ($null -eq $Resolution) { return $null }
        $Canonical = $Resolution.Path
    }
    catch {
        return $null
    }
    $Candidates = @($Lexical, $Canonical) + @($Resolution.Traversed)
    foreach ($Root in $CheckoutRoots) {
        foreach ($Candidate in $Candidates) {
            if ($Candidate -ieq $Root -or $Candidate.StartsWith($Root + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
                return $null
            }
        }
    }
    return $Lexical
}

function Get-ExternalApplications([string] $Name) {
    Get-Command $Name -CommandType Application -All -ErrorAction SilentlyContinue |
        ForEach-Object {
            $ExternalPath = Get-ExternalApplicationPath $_.Source
            if ($null -ne $ExternalPath) { $ExternalPath }
        } |
        Select-Object -Unique
}
$ForwardedArgs = @()

if ($Help) {
    $ForwardedArgs += '--help'
}

if ($PSBoundParameters.ContainsKey('Fixture')) {
    $ForwardedArgs += @('--fixture', $Fixture)
}

if ($PSBoundParameters.ContainsKey('Engine')) {
    foreach ($SelectedEngine in $Engine) {
        $ForwardedArgs += @('--engine', $SelectedEngine)
    }
}

foreach ($PyLauncher in Get-ExternalApplications 'py') {
    & $PyLauncher -3 -I -c 'import sys; raise SystemExit(sys.version_info < (3, 9))' *> $null
    if ($LASTEXITCODE -eq 0) {
        & $PyLauncher -3 -I $Harness @ForwardedArgs
        exit $LASTEXITCODE
    }
}

foreach ($Python in Get-ExternalApplications 'python') {
    & $Python -I -c 'import sys; raise SystemExit(sys.version_info < (3, 9))' *> $null
    if ($LASTEXITCODE -eq 0) {
        & $Python -I $Harness @ForwardedArgs
        exit $LASTEXITCODE
    }
}

[Console]::Error.WriteLine('Python 3.9 or newer outside the reviewed checkout is required to run test-review-harness.')
exit 127
