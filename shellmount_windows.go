//go:build windows

package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// shellMount implements "mount.exe shell-mount", which the Explorer context
// menu runs with no console:
//
//	mount.exe shell-mount -drive X: [-ro] <image>
//
// It applies the per-user defaults, starts the real mount as a hidden
// background process, waits for the drive to appear and opens it in
// Explorer. Errors are shown in a message box, since there is no console.
//
// Defaults:
//   - writable mounts use %USERPROFILE%\squashoverlay\overlays\<image name>
//   - images on a network drive or UNC path use the disk cache in
//     %LOCALAPPDATA%\squashoverlay\cache
//   - the volume label is the image name
//   - output goes to %LOCALAPPDATA%\squashoverlay\logs\<letter>.log
func shellMount(args []string) int {
	fl := flag.NewFlagSet("shell-mount", flag.ContinueOnError)
	drive := fl.String("drive", "", "drive letter, e.g. R:")
	readOnly := fl.Bool("ro", false, "mount read-only")
	if err := fl.Parse(args); err != nil || fl.NArg() != 1 || *drive == "" {
		return shellError("usage: mount.exe shell-mount -drive X: [-ro] <image>")
	}
	image, err := filepath.Abs(fl.Arg(0))
	if err != nil {
		return shellError(err.Error())
	}
	letter := normalizeDrive(*drive)

	if d := mountedDriveFor(image); d != "" {
		openInExplorer(d)
		return 0
	}
	if driveInUse(letter) {
		return shellError(fmt.Sprintf("%s is already in use. Pick another letter.", letter))
	}

	name := strings.TrimSuffix(filepath.Base(image), filepath.Ext(image))
	mountArgs := []string{"-drive", letter, "-label", name}
	if !*readOnly {
		home, _ := os.UserHomeDir()
		mountArgs = append(mountArgs, "-overlay", filepath.Join(home, "squashoverlay", "overlays", name))
	}
	if isRemotePath(image) {
		mountArgs = append(mountArgs, "-disk-cache", filepath.Join(stateDir(), "cache"))
	}
	mountArgs = append(mountArgs, image)

	logPath := filepath.Join(stateDir(), "logs", strings.TrimSuffix(letter, ":")+".log")
	os.MkdirAll(filepath.Dir(logPath), 0o755)
	logFile, err := os.Create(logPath)
	if err != nil {
		return shellError(err.Error())
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "%s mount.exe %s\r\n", time.Now().Format(time.RFC3339), strings.Join(mountArgs, " "))

	exe, err := os.Executable()
	if err != nil {
		return shellError(err.Error())
	}
	cmd := exec.Command(exe, mountArgs...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return shellError(err.Error())
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()

	deadline := time.After(2 * time.Minute)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-exited:
			return shellError(fmt.Sprintf("Mounting %s failed.\n\n%s", filepath.Base(image), logTail(logPath, 15)))
		case <-deadline:
			return shellError(fmt.Sprintf("%s did not appear after 2 minutes. The mount may still be starting; see %s.", letter, logPath))
		case <-tick.C:
			if _, err := os.Stat(mountStatePath(letter)); err == nil {
				if _, err := os.Stat(letter + `\`); err == nil {
					openInExplorer(letter)
					return 0
				}
			}
		}
	}
}

func shellError(msg string) int {
	windows.MessageBox(0, windows.StringToUTF16Ptr(msg), windows.StringToUTF16Ptr("squashoverlay"),
		windows.MB_OK|windows.MB_ICONERROR)
	return 1
}

func openInExplorer(drive string) {
	exec.Command("explorer.exe", drive+`\`).Start()
}

func driveInUse(letter string) bool {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return false
	}
	return mask&(1<<uint(letter[0]-'A')) != 0
}

// isRemotePath reports whether p is on a network share.
func isRemotePath(p string) bool {
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	root := filepath.VolumeName(p) + `\`
	return windows.GetDriveType(windows.StringToUTF16Ptr(root)) == windows.DRIVE_REMOTE
}

// mountedDriveFor returns the drive where image is mounted by a live
// process, or "". State files of processes that are gone are removed.
func mountedDriveFor(image string) string {
	files, _ := filepath.Glob(filepath.Join(stateDir(), "mounts", "*.txt"))
	for _, f := range files {
		st := readMountState(f)
		pid, _ := strconv.Atoi(st["pid"])
		if !processAlive(pid) {
			os.Remove(f)
			continue
		}
		if strings.EqualFold(st["image"], image) {
			return strings.TrimSuffix(filepath.Base(f), ".txt") + ":"
		}
	}
	return ""
}

func readMountState(path string) map[string]string {
	st := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return st
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok {
			st[k] = v
		}
	}
	return st
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

func logTail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
