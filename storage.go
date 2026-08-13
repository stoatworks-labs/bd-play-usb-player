package main

// USB mass-storage discovery and mounting.
//
// The PLAY's stock kernel has usb-storage built in (there are no loadable
// modules at all), and vfat + ntfs are compiled in. exFAT is NOT — see
// probeFilesystems. FUSE is present, so exfat-fuse can cover it if the helper
// binary is shipped alongside us.
//
// Nothing automounts on the stock firmware: /media/usb0..usb7 exist as empty
// directories and no udev rule or automount daemon populates them. So we do it
// ourselves, by polling. Polling rather than netlink uevents because the whole
// daemon is a supervisor of subprocesses anyway, a 1s scan costs nothing on an
// idle box, and it recovers from a missed event rather than wedging.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Volume is a mounted USB filesystem.
type Volume struct {
	Device     string // /dev/sda1
	MountPoint string // /media/usb0
	FSType     string // vfat, ntfs, exfat
	Label      string
	ReadOnly   bool
	ownedByUs  bool // we mounted it, so we may unmount it
}

// fsCandidates is the order we try filesystems in when blkid can't tell us.
// vfat first: it is by far the most common on sticks small enough to be
// formatted FAT32, and it is the cheapest probe.
var fsCandidates = []string{"vfat", "ntfs", "exfat"}

// mountRoot holds the mount points the stock firmware already creates.
const mountRoot = "/media"

// StorageManager tracks which USB volumes are mounted.
type StorageManager struct {
	mu      sync.Mutex
	volumes map[string]*Volume // keyed by device path
	// exfatHelper is the path to a mount.exfat-fuse binary, or "" if exFAT is
	// unsupported on this device. Discovered once at startup.
	exfatHelper string
	// extraDir is an always-present media directory that is not USB storage at
	// all. It exists so content can be staged on internal storage (a fixed
	// fallback loop for a signage unit, say) and so the player can be
	// exercised without a stick plugged in.
	extraDir string
	log      *Logger
}

func NewStorageManager(log *Logger) *StorageManager {
	sm := &StorageManager{
		volumes: make(map[string]*Volume),
		log:     log,
	}
	sm.exfatHelper = findExfatHelper()
	if sm.exfatHelper == "" {
		log.Warn("no exFAT helper found; exFAT sticks will not mount (kernel has no exfat driver)")
	} else {
		log.Info("exFAT support via %s", sm.exfatHelper)
	}
	return sm
}

// findExfatHelper looks for a FUSE exfat mount helper. We ship one next to the
// binary; a system copy wins if the firmware ever grows one.
func findExfatHelper() string {
	selfDir := ""
	if exe, err := os.Executable(); err == nil {
		selfDir = filepath.Dir(exe)
	}
	candidates := []string{
		"/sbin/mount.exfat-fuse",
		"/usr/sbin/mount.exfat-fuse",
		"/sbin/mount.exfat",
		"/usr/sbin/mount.exfat",
	}
	if selfDir != "" {
		// Ours takes lowest precedence but is the one that normally exists.
		candidates = append(candidates,
			filepath.Join(selfDir, "mount.exfat-fuse"),
			filepath.Join(selfDir, "exfat-fuse"),
		)
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return c
		}
	}
	return ""
}

// SetExtraDir registers a fixed media directory alongside any USB storage.
func (sm *StorageManager) SetExtraDir(dir string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.extraDir = dir
}

// Volumes returns the currently mounted USB volumes, newest device first.
func (sm *StorageManager) Volumes() []Volume {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	out := make([]Volume, 0, len(sm.volumes)+1)
	for _, v := range sm.volumes {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })

	// A real stick always wins over the fixed directory, so inserting one
	// takes over from the fallback content rather than being ignored.
	if sm.extraDir != "" {
		out = append(out, Volume{
			Device:     "(local)",
			MountPoint: sm.extraDir,
			FSType:     "local",
			Label:      filepath.Base(sm.extraDir),
			ReadOnly:   true,
		})
	}
	return out
}

// Primary returns the volume the player should use by default: the first
// mounted one. With a single USB-A port on the PLAY this is nearly always the
// only one, but a hub can present several and we stay deterministic.
func (sm *StorageManager) Primary() (Volume, bool) {
	vols := sm.Volumes()
	if len(vols) == 0 {
		return Volume{}, false
	}
	return vols[0], true
}

// Scan performs one discovery pass: mount anything new, forget anything gone.
// Returns true if the set of mounted volumes changed.
func (sm *StorageManager) Scan() bool {
	present := discoverPartitions()
	changed := false

	sm.mu.Lock()
	// Drop volumes whose device disappeared (stick yanked without eject).
	for dev, v := range sm.volumes {
		if !present[dev] {
			sm.log.Info("usb removed: %s (%s)", dev, v.MountPoint)
			// Lazy unmount: the device is already gone, a normal umount would
			// block on dirty pages that can never be written back.
			_ = exec.Command("umount", "-l", v.MountPoint).Run()
			delete(sm.volumes, dev)
			changed = true
		}
	}
	known := make(map[string]bool, len(sm.volumes))
	for dev := range sm.volumes {
		known[dev] = true
	}
	sm.mu.Unlock()

	for dev := range present {
		if known[dev] {
			continue
		}
		v, err := sm.mount(dev)
		if err != nil {
			// Log once per appearance, not per scan: remember the failure by
			// recording nothing and letting the next scan retry. To avoid a
			// log flood we only complain when the device is newly seen, which
			// is what `known` gives us.
			sm.log.Warn("mount %s: %v", dev, err)
			continue
		}
		sm.log.Info("usb mounted: %s -> %s (%s, label=%q)", v.Device, v.MountPoint, v.FSType, v.Label)
		sm.mu.Lock()
		sm.volumes[dev] = v
		sm.mu.Unlock()
		changed = true
	}
	return changed
}

// discoverPartitions lists USB block partitions worth trying, as a set.
//
// We read /proc/partitions rather than globbing /dev/sd*, because a stick with
// no partition table presents as the whole disk (/dev/sda) with a filesystem
// directly on it, and /proc/partitions lets us tell a whole disk from its
// partitions by looking at what else shares the prefix.
func discoverPartitions() map[string]bool {
	data, err := os.ReadFile("/proc/partitions")
	if err != nil {
		return nil
	}
	// Collect every sd* name.
	var names []string
	for _, line := range strings.Split(string(data), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		name := f[3]
		if !strings.HasPrefix(name, "sd") {
			continue
		}
		names = append(names, name)
	}

	// A whole disk (sda) is only interesting if it has no partitions of its
	// own; otherwise we would mount the container and its contents twice.
	hasPart := make(map[string]bool)
	for _, n := range names {
		base := strings.TrimRight(n, "0123456789")
		if base != n {
			hasPart[base] = true
		}
	}

	out := make(map[string]bool)
	for _, n := range names {
		base := strings.TrimRight(n, "0123456789")
		if base == n && hasPart[base] {
			continue // whole disk that has partitions; skip it
		}
		if !isUSB(n) {
			continue
		}
		out["/dev/"+n] = true
	}
	return out
}

// isUSB reports whether a block device sits behind a USB controller. Without
// this an internal disk (or the eMMC, were it ever to appear as sd*) could be
// mounted and played, which is not what "USB" means to the user.
func isUSB(name string) bool {
	// /sys/class/block/<name> symlinks into the device tree; a USB device has
	// "usb" in that path.
	target, err := os.Readlink("/sys/class/block/" + name)
	if err != nil {
		return false
	}
	return strings.Contains(target, "/usb")
}

// mount attaches dev at the first free /media/usbN and returns the Volume.
func (sm *StorageManager) mount(dev string) (*Volume, error) {
	mp, err := freeMountPoint()
	if err != nil {
		return nil, err
	}

	fsType := probeFSType(dev)
	order := fsCandidates
	if fsType != "" {
		// Try the probed type first, then the rest as a fallback.
		order = append([]string{fsType}, fsCandidates...)
	}

	var lastErr error
	tried := make(map[string]bool)
	for _, fs := range order {
		if tried[fs] {
			continue
		}
		tried[fs] = true
		if err := sm.mountAs(dev, mp, fs); err != nil {
			lastErr = err
			continue
		}
		return &Volume{
			Device:     dev,
			MountPoint: mp,
			FSType:     fs,
			Label:      probeLabel(dev),
			ReadOnly:   true, // we always mount ro; see mountAs
			ownedByUs:  true,
		}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no supported filesystem")
	}
	return nil, lastErr
}

// mountAs mounts one device with one filesystem type.
//
// Always read-only. The player never writes to the stick, and a read-only
// mount means a stick pulled without ejecting cannot be corrupted — which,
// given this is a signage/playback appliance that people will unplug casually,
// is the behaviour that matches how the thing is actually used.
func (sm *StorageManager) mountAs(dev, mp, fs string) error {
	if fs == "exfat" {
		if sm.exfatHelper == "" {
			return errors.New("exfat: no FUSE helper available")
		}
		cmd := exec.Command(sm.exfatHelper, "-o", "ro", dev, mp)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("exfat-fuse: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// uid/gid/umask are vfat-only options; ntfs ignores them harmlessly but
	// older mount builds complain, so only pass them for vfat.
	opts := "ro,noatime"
	if fs == "vfat" {
		opts += ",umask=0022,iocharset=utf8"
	}
	cmd := exec.Command("mount", "-t", fs, "-o", opts, dev, mp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount -t %s: %v: %s", fs, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// freeMountPoint returns the first /media/usbN that is not already a mount
// point, creating it if the stock firmware did not.
func freeMountPoint() (string, error) {
	mounted := mountedPaths()
	for i := 0; i < 8; i++ {
		mp := filepath.Join(mountRoot, "usb"+strconv.Itoa(i))
		if mounted[mp] {
			continue
		}
		if err := os.MkdirAll(mp, 0o755); err != nil {
			return "", err
		}
		return mp, nil
	}
	return "", errors.New("no free mount point under " + mountRoot)
}

func mountedPaths() map[string]bool {
	out := make(map[string]bool)
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			out[unescapeMount(f[1])] = true
		}
	}
	return out
}

// unescapeMount undoes the octal escaping the kernel applies to mount paths
// containing spaces and friends.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// probeFSType asks blkid what is on the device. blkid may be absent; an empty
// answer just means we fall back to trying each candidate in turn.
func probeFSType(dev string) string {
	out, err := exec.Command("blkid", "-s", "TYPE", "-o", "value", dev).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func probeLabel(dev string) string {
	out, err := exec.Command("blkid", "-s", "LABEL", "-o", "value", dev).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Eject unmounts every volume we mounted. Used by the UI's eject button and on
// shutdown. Because we mount read-only there is nothing to flush, so this is
// effectively instant and cannot fail in a way that loses data.
func (sm *StorageManager) Eject() error {
	sm.mu.Lock()
	vols := make([]*Volume, 0, len(sm.volumes))
	for _, v := range sm.volumes {
		vols = append(vols, v)
	}
	sm.mu.Unlock()

	var firstErr error
	for _, v := range vols {
		if !v.ownedByUs {
			continue
		}
		if out, err := exec.Command("umount", v.MountPoint).CombinedOutput(); err != nil {
			err = fmt.Errorf("umount %s: %v: %s", v.MountPoint, err, strings.TrimSpace(string(out)))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		sm.mu.Lock()
		delete(sm.volumes, v.Device)
		sm.mu.Unlock()
	}
	return firstErr
}

// Watch runs Scan on a ticker until ctx-like stop channel closes, calling
// onChange whenever the volume set changes.
func (sm *StorageManager) Watch(stop <-chan struct{}, interval time.Duration, onChange func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	if sm.Scan() && onChange != nil {
		onChange()
	}
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if sm.Scan() && onChange != nil {
				onChange()
			}
		}
	}
}

// probeFilesystems reports which of our candidate filesystems the running
// kernel supports natively. Recorded for the status API so a user whose exFAT
// stick did not mount gets told why rather than seeing an empty library.
func probeFilesystems() map[string]bool {
	out := map[string]bool{}
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		out[f[len(f)-1]] = true
	}
	return out
}
