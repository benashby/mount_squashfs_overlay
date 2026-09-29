<#
.SYNOPSIS
Installs mount.exe and the Explorer context menu.

.DESCRIPTION
Right-clicking a .sqfs, .squashfs or .wsquashfs file then offers "Mount as"
and "Mount read-only as"; right-clicking a mounted drive offers "Unmount".
On Windows 11 the entries are under "Show more options" unless the classic
menu is enabled.

With UAC on, the menu can be installed for the current user only, with no
administrator rights: files go to %LOCALAPPDATA%\Programs\squashoverlay and
the registration to HKCU. With UAC off, Explorer runs with a full
administrator token and ignores per-user shell extensions, so the menu has to
be installed for the machine: files go to %ProgramFiles%\squashoverlay and
the registration to HKLM, which needs an administrator. -Scope Auto (the
default) picks Machine when UAC is off and User otherwise. The registration
in the other scope is removed so the entries never show twice.

Running it again updates the installed files. Files in use by Explorer or a
running mount are renamed aside and removed on a later run.

.PARAMETER MountExe
Path to mount.exe. Defaults to mount.exe in the repository root.

.PARAMETER ShellDll
Path to squashoverlay_shell.dll. Defaults to shell\build\squashoverlay_shell.dll.

.PARAMETER Scope
Auto, Machine or User.

.PARAMETER RestartExplorer
Restart Explorer so it loads an updated DLL right away.
#>
param(
    [string]$MountExe = (Join-Path $PSScriptRoot '..\mount.exe'),
    [string]$ShellDll = (Join-Path $PSScriptRoot '..\shell\build\squashoverlay_shell.dll'),
    [ValidateSet('Auto', 'Machine', 'User')]
    [string]$Scope = 'Auto',
    [switch]$RestartExplorer
)

$ErrorActionPreference = 'Stop'
$clsid = '{0ED6EEBA-7DB7-4DE4-B8B0-1D355E1EE80D}'

$uacOn = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System' -ErrorAction SilentlyContinue).EnableLUA -ne 0
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)
if ($Scope -eq 'Auto') { $Scope = if ($uacOn) { 'User' } else { 'Machine' } }
if ($Scope -eq 'User' -and -not $uacOn) {
    Write-Warning 'UAC is off, so Explorer ignores per-user shell extensions. Use -Scope Machine.'
}
if ($Scope -eq 'Machine' -and -not $isAdmin) {
    throw 'Installing for the machine needs an administrator PowerShell.'
}

$scopes = @{
    Machine = @{ Dest = Join-Path $env:ProgramFiles 'squashoverlay'; Classes = 'HKLM:\SOFTWARE\Classes' }
    User    = @{ Dest = Join-Path $env:LOCALAPPDATA 'Programs\squashoverlay'; Classes = 'HKCU:\Software\Classes' }
}
$dest = $scopes[$Scope].Dest
$classes = $scopes[$Scope].Classes
$otherScope = if ($Scope -eq 'Machine') { 'User' } else { 'Machine' }

# Keys the menu is registered under, relative to a Classes root.
$handlerKeys = @(
    '*\shellex\ContextMenuHandlers\squashoverlay',      # every file; the DLL only acts on images
    'Drive\shellex\ContextMenuHandlers\squashoverlay'
)
# Registered by earlier versions of this script.
$legacyKeys = '.sqfs', '.squashfs', '.wsquashfs' | ForEach-Object {
    "SystemFileAssociations\$_\shellex\ContextMenuHandlers\squashoverlay"
}

function Remove-Registration([string]$Root) {
    foreach ($k in $handlerKeys + $legacyKeys) {
        Remove-Item -LiteralPath "$Root\$k" -ErrorAction SilentlyContinue
    }
    Remove-Item -LiteralPath "$Root\CLSID\$clsid" -Recurse -ErrorAction SilentlyContinue
}

foreach ($f in $MountExe, $ShellDll) {
    if (-not (Test-Path $f)) { throw "Not found: $f (build it first)" }
}
New-Item -ItemType Directory -Force $dest | Out-Null

# Remove files renamed aside by earlier runs once nothing holds them.
Get-ChildItem $dest -Filter '*.old-*' -ErrorAction SilentlyContinue | ForEach-Object {
    try { [IO.File]::Delete($_.FullName) } catch { }
}

function Install-File([string]$Source, [string]$Name) {
    $target = Join-Path $dest $Name
    if (Test-Path $target) {
        try {
            [IO.File]::Delete($target)
        } catch {
            # Loaded by Explorer or a running mount. Windows allows renaming it.
            Rename-Item $target "$Name.old-$(Get-Date -Format yyyyMMddHHmmss)"
        }
    }
    Copy-Item $Source $target
    $target
}

$exe = Install-File (Resolve-Path $MountExe) 'mount.exe'
$dll = Install-File (Resolve-Path $ShellDll) 'squashoverlay_shell.dll'

Remove-Registration $classes
$server = New-Item -Force "$classes\CLSID\$clsid\InprocServer32"
Set-Item -LiteralPath "$classes\CLSID\$clsid" 'squashoverlay context menu'
Set-Item -LiteralPath $server.PSPath $dll
Set-ItemProperty -LiteralPath $server.PSPath ThreadingModel Apartment
foreach ($k in $handlerKeys) {
    $key = New-Item -Force -Path "$classes\$k"
    Set-Item -LiteralPath $key.PSPath $clsid
}

if ($otherScope -eq 'User' -or $isAdmin) {
    Remove-Registration $scopes[$otherScope].Classes
}

# Tell Explorer that handler registrations changed.
Add-Type -Namespace SquashOverlay -Name Shell -MemberDefinition @'
[DllImport("shell32.dll")]
public static extern void SHChangeNotify(int eventId, uint flags, System.IntPtr item1, System.IntPtr item2);
'@
[SquashOverlay.Shell]::SHChangeNotify(0x08000000, 0, [IntPtr]::Zero, [IntPtr]::Zero)  # SHCNE_ASSOCCHANGED

if ($RestartExplorer) {
    Get-Process explorer -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep 2
    if (-not (Get-Process explorer -ErrorAction SilentlyContinue)) { Start-Process explorer.exe }
}

Write-Host "Installed for the $($Scope.ToLower()) to $dest"
Write-Host 'Right-click a .sqfs/.squashfs/.wsquashfs file to mount it.'
