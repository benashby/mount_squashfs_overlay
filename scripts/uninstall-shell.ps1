<#
.SYNOPSIS
Removes the Explorer context menu and the installed mount.exe.

.DESCRIPTION
Deletes the registration made by install-shell.ps1 for the current user and,
when run as an administrator, for the machine, and removes the installed
files. Mounted drives stay mounted until unmounted or until you sign out.
Overlays in %USERPROFILE%\squashoverlay and the disk cache in
%LOCALAPPDATA%\squashoverlay\cache are kept; delete those folders yourself if
you no longer want them.
#>
$ErrorActionPreference = 'Stop'
$clsid = '{0ED6EEBA-7DB7-4DE4-B8B0-1D355E1EE80D}'
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)

$keys = @('*\shellex\ContextMenuHandlers\squashoverlay', 'Drive\shellex\ContextMenuHandlers\squashoverlay') +
    ('.sqfs', '.squashfs', '.wsquashfs' | ForEach-Object { "SystemFileAssociations\$_\shellex\ContextMenuHandlers\squashoverlay" })

$targets = @(@{ Classes = 'HKCU:\Software\Classes'; Dest = Join-Path $env:LOCALAPPDATA 'Programs\squashoverlay' })
if ($isAdmin) {
    $targets += @{ Classes = 'HKLM:\SOFTWARE\Classes'; Dest = Join-Path $env:ProgramFiles 'squashoverlay' }
} elseif (Test-Path "HKLM:\SOFTWARE\Classes\CLSID\$clsid") {
    Write-Warning 'A machine-wide installation exists; run this from an administrator PowerShell to remove it.'
}

foreach ($t in $targets) {
    foreach ($k in $keys) { Remove-Item -LiteralPath "$($t.Classes)\$k" -ErrorAction SilentlyContinue }
    Remove-Item -LiteralPath "$($t.Classes)\CLSID\$clsid" -Recurse -ErrorAction SilentlyContinue
    Get-ChildItem $t.Dest -File -ErrorAction SilentlyContinue | ForEach-Object {
        $file = $_
        try {
            [IO.File]::Delete($file.FullName)
        } catch {
            Write-Warning "$($file.Name) is in use; it will be left in $($t.Dest). Sign out or restart Explorer, then delete it."
        }
    }
}
Write-Host 'Context menu removed.'
