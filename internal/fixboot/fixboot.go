package fixboot

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const mountRoot = "/mnt/clonefix"

// Config holds the fix-boot parameters.
type Config struct {
	TargetDisk string // e.g. /dev/sda
	Verbose    bool
}

// Run performs post-clone boot repair:
//  1. Re-read partition table
//  2. Activate LVM if present
//  3. Find and mount root filesystem
//  4. Parse fstab to mount /boot, /boot/efi
//  5. Bind-mount /dev, /proc, /sys, /run
//  6. Rebuild initramfs (dracut / update-initramfs / mkinitcpio)
//  7. Reinstall GRUB if needed
//  8. Clean up
func Run(cfg Config) error {
	log := func(format string, args ...interface{}) {
		fmt.Printf("  "+format+"\n", args...)
	}

	// ── 0. Prepare mount root ──────────────────────────────────────
	if err := os.MkdirAll(mountRoot, 0755); err != nil {
		return fmt.Errorf("create %s: %w", mountRoot, err)
	}

	// ── 1. Re-read partition table ─────────────────────────────────
	log("刷新分区??..")
	run("partprobe", cfg.TargetDisk)
	run("blockdev", "--rereadpt", cfg.TargetDisk)
	time.Sleep(2 * time.Second)

	// Create device nodes ??Alpine uses mdev instead of udev,
	// partition nodes like /dev/sda1 are NOT auto-created.
	// mdev -s does a coldplug scan: reads /sys and creates all /dev nodes.
	if commandExists("mdev") {
		runQuiet("mdev", "-s")
	} else {
		// Fallback: manually create partition nodes from /sys
		ensurePartitionNodes(cfg.TargetDisk)
	}

	// ── 2. Activate LVM if available ───────────────────────────────
	if commandExists("lvm") {
		log("检??LVM...")
		run("vgscan", "--mknodes")
		run("vgchange", "-ay")
		time.Sleep(time.Second)
	} else {
		log("未检测到 lvm 工具 (如需 LVM 支持: apk add lvm2)")
	}

	// ── 3. Find root filesystem ────────────────────────────────────
	log("查找根文件系??..")
	rootDev, rootFstype, err := findRootPartition(cfg.TargetDisk)
	if err != nil {
		return fmt.Errorf("找不到根文件系统: %w\n  请手动指定分区并挂载??%s", err, mountRoot)
	}
	log("  根分?? %s (%s)", rootDev, rootFstype)

	// Mount root
	if err := mountWithType(rootDev, mountRoot, rootFstype); err != nil {
		// Fallback to auto-detect
		if err2 := mount(rootDev, mountRoot); err2 != nil {
			return fmt.Errorf("挂载根分??%s: %w", rootDev, err)
		}
	}
	defer umountAll()

	// ── 4. Detect distro ───────────────────────────────────────────
	distro := detectDistro(mountRoot)
	log("  系统类型: %s", distro)

	// ── 5. Parse fstab and mount /boot, /boot/efi ──────────────────
	fstabMounts := parseFstab(filepath.Join(mountRoot, "etc/fstab"))

	if dev, ok := fstabMounts["/boot"]; ok {
		bootDir := filepath.Join(mountRoot, "boot")
		log("  挂载 /boot ??%s", dev)
		if err := mount(dev, bootDir); err != nil {
			log("  ??挂载 /boot 失败: %v (跳过)", err)
		}
	}

	if dev, ok := fstabMounts["/boot/efi"]; ok {
		efiDir := filepath.Join(mountRoot, "boot/efi")
		os.MkdirAll(efiDir, 0755)
		log("  挂载 /boot/efi ??%s", dev)
		if err := mount(dev, efiDir); err != nil {
			log("  ??挂载 /boot/efi 失败: %v (跳过)", err)
		}
	}

	// ── 6. Bind-mount virtual filesystems ──────────────────────────
	log("挂载虚拟文件系统...")
	bindPairs := []struct{ src, dst string }{
		{"/dev", filepath.Join(mountRoot, "dev")},
		{"/dev/pts", filepath.Join(mountRoot, "dev/pts")},
		{"/proc", filepath.Join(mountRoot, "proc")},
		{"/sys", filepath.Join(mountRoot, "sys")},
	}
	for _, bp := range bindPairs {
		os.MkdirAll(bp.dst, 0755)
		if err := mountBind(bp.src, bp.dst); err != nil {
			return fmt.Errorf("bind mount %s: %w", bp.src, err)
		}
	}

	// Mount /run as tmpfs inside chroot
	runDir := filepath.Join(mountRoot, "run")
	os.MkdirAll(runDir, 0755)
	mountTmpfs(runDir)

	// ── 7. Rebuild initramfs ───────────────────────────────────────
	log("重建 initramfs (包含所有硬件驱??...")
	switch distro {
	case "fedora", "rhel", "centos", "rocky", "alma":
		err = chrootExec(mountRoot, "dracut", "--no-hostonly", "--regenerate-all", "--force")
	case "debian", "ubuntu", "linuxmint":
		err = chrootExec(mountRoot, "update-initramfs", "-u", "-k", "all")
	case "arch", "manjaro":
		err = chrootExec(mountRoot, "mkinitcpio", "-P")
	case "opensuse", "suse":
		err = chrootExec(mountRoot, "dracut", "--no-hostonly", "--regenerate-all", "--force")
	default:
		// Try dracut first (most common for enterprise distros)
		if fileExists(filepath.Join(mountRoot, "usr/bin/dracut")) ||
			fileExists(filepath.Join(mountRoot, "usr/sbin/dracut")) {
			err = chrootExec(mountRoot, "dracut", "--no-hostonly", "--regenerate-all", "--force")
		} else if fileExists(filepath.Join(mountRoot, "usr/sbin/update-initramfs")) {
			err = chrootExec(mountRoot, "update-initramfs", "-u", "-k", "all")
		} else if fileExists(filepath.Join(mountRoot, "usr/bin/mkinitcpio")) {
			err = chrootExec(mountRoot, "mkinitcpio", "-P")
		} else {
			return fmt.Errorf("未找??initramfs 重建工具 (dracut/update-initramfs/mkinitcpio)")
		}
	}
	if err != nil {
		return fmt.Errorf("重建 initramfs 失败: %w", err)
	}
	log("  ??initramfs 重建完成")

	// ── 8. Reinstall GRUB ──────────────────────────────────────────
	log("修复 GRUB 引导...")
	efiDir := filepath.Join(mountRoot, "boot/efi/EFI")
	isEFI := dirExists(efiDir)

	if isEFI {
		log("  检测到 UEFI 模式")
		// Fedora/RHEL
		if fileExists(filepath.Join(mountRoot, "usr/sbin/grub2-install")) {
			err = chrootExec(mountRoot, "grub2-install",
				"--target=x86_64-efi",
				"--efi-directory=/boot/efi",
				"--bootloader-id=fedora",
				"--recheck")
			if err != nil {
				log("  ??grub2-install 失败: %v (可能需要手动处??", err)
			}
			chrootExec(mountRoot, "grub2-mkconfig", "-o", "/boot/grub2/grub.cfg")
		} else if fileExists(filepath.Join(mountRoot, "usr/sbin/grub-install")) {
			err = chrootExec(mountRoot, "grub-install",
				"--target=x86_64-efi",
				"--efi-directory=/boot/efi",
				"--recheck")
			if err != nil {
				log("  ??grub-install 失败: %v", err)
			}
			chrootExec(mountRoot, "grub-mkconfig", "-o", "/boot/grub/grub.cfg")
		}

		// Add UEFI boot entry if efibootmgr is available
		if commandExists("efibootmgr") {
			efiPart := findEFIPartNum(cfg.TargetDisk, fstabMounts)
			if efiPart != "" {
				shimPath := findShimPath(mountRoot)
				if shimPath != "" {
					run("efibootmgr", "-c",
						"-d", cfg.TargetDisk,
						"-p", efiPart,
						"-L", "Linux",
						"-l", shimPath)
					log("  ??UEFI 引导项已添加")
				}
			}
		} else {
			log("efibootmgr not found, UEFI boot entry may need manual setup")
			log("    Install on Alpine: apk add efibootmgr")
		}
	} else {
		log("  检测到 BIOS/Legacy 模式")
		if fileExists(filepath.Join(mountRoot, "usr/sbin/grub2-install")) {
			err = chrootExec(mountRoot, "grub2-install", "--recheck", cfg.TargetDisk)
			if err != nil {
				log("  ??grub2-install 失败: %v", err)
			}
			chrootExec(mountRoot, "grub2-mkconfig", "-o", "/boot/grub2/grub.cfg")
		} else if fileExists(filepath.Join(mountRoot, "usr/sbin/grub-install")) {
			err = chrootExec(mountRoot, "grub-install", "--recheck", cfg.TargetDisk)
			if err != nil {
				log("  ??grub-install 失败: %v", err)
			}
			chrootExec(mountRoot, "grub-mkconfig", "-o", "/boot/grub/grub.cfg")
		}
	}
	log("  ??GRUB 修复完成")

	// ── 9. Fix fstab: remove extra disk mounts ─────────────────────
	log("修复 fstab (移除不存在的额外磁盘挂载)...")
	if err := fixFstab(mountRoot); err != nil {
		log("  ??fstab 修复失败: %v (不影响启?? 可手动处??", err)
		_ = copyFile(
			filepath.Join(mountRoot, "etc/fstab"),
			filepath.Join(mountRoot, "etc/fstab.bak"),
		)
	}

	// ── 10. Cleanup ─────────────────────────────────────────────────
	log("清理挂载??..")
	umountAll()
	log("??引导修复完成!")

	return nil
}

// ─── Helpers ───────────────────────────────────────────────────────

func findRootPartition(targetDisk string) (string, string, error) {
	// Strategy: use blkid to detect partitions and their types,
	// then try mounting the largest ones first (most likely root).

	candidates := []string{}

	// 1. Direct partitions on target disk (sda1, sda2, nvme0n1p1, etc.)
	entries, _ := filepath.Glob(targetDisk + "*")
	for _, e := range entries {
		if e != targetDisk {
			candidates = append(candidates, e)
		}
	}
	// NVMe style
	if strings.Contains(targetDisk, "nvme") || strings.Contains(targetDisk, "loop") {
		pEntries, _ := filepath.Glob(targetDisk + "p*")
		for _, e := range pEntries {
			candidates = append(candidates, e)
		}
	}

	// 2. LVM logical volumes
	lvmDevs, _ := filepath.Glob("/dev/mapper/*")
	for _, d := range lvmDevs {
		if d != "/dev/mapper/control" {
			candidates = append(candidates, d)
		}
	}
	dmDevs, _ := filepath.Glob("/dev/dm-*")
	candidates = append(candidates, dmDevs...)

	// De-duplicate
	seen := map[string]bool{}
	unique := []string{}
	for _, c := range candidates {
		real, err := filepath.EvalSymlinks(c)
		if err != nil {
			real = c
		}
		if !seen[real] {
			seen[real] = true
			unique = append(unique, c)
		}
	}

	if len(unique) == 0 {
		return "", "", fmt.Errorf("no partitions found on %s", targetDisk)
	}

	// Ensure common filesystem kernel modules are loaded
	for _, mod := range []string{"ext4", "xfs", "btrfs", "vfat"} {
		runQuiet("modprobe", mod)
	}

	tmpMount := mountRoot + ".probe"
	os.MkdirAll(tmpMount, 0755)
	defer func() {
		umountSingle(tmpMount)
		os.Remove(tmpMount)
	}()

	// Sort candidates: try larger partitions first (more likely to be root)
	// Use blkid to get filesystem info
	type partInfo struct {
		dev    string
		fstype string
	}
	var parts []partInfo
	for _, dev := range unique {
		fs := getBlkidFstype(dev)
		parts = append(parts, partInfo{dev: dev, fstype: fs})
	}

	// Try partitions with known Linux filesystems first (ext4, xfs, btrfs)
	// Skip swap, vfat (usually EFI/boot), and unknown
	linuxFS := map[string]bool{"ext4": true, "ext3": true, "ext2": true, "xfs": true, "btrfs": true}
	ordered := []partInfo{}
	var rest []partInfo
	for _, p := range parts {
		if linuxFS[p.fstype] {
			ordered = append(ordered, p)
		} else {
			rest = append(rest, p)
		}
	}
	ordered = append(ordered, rest...)

	for _, p := range ordered {
		if p.fstype == "swap" || p.fstype == "" {
			continue
		}

		var mountErr error
		if p.fstype != "" {
			mountErr = mountWithType(p.dev, tmpMount, p.fstype)
		} else {
			mountErr = mount(p.dev, tmpMount)
		}
		if mountErr != nil {
			continue
		}

		if isRootFS(tmpMount) {
			umountSingle(tmpMount)
			return p.dev, p.fstype, nil
		}
		umountSingle(tmpMount)
	}

	return "", "", fmt.Errorf("no partition contains a Linux root filesystem")
}

// getBlkidFstype returns the filesystem type of a device using blkid.
func getBlkidFstype(dev string) string {
	out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", dev).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func isRootFS(mountpoint string) bool {
	markers := []string{
		"etc/os-release",
		"usr/lib/modules",
		"usr/bin",
	}
	for _, m := range markers {
		if !fileExists(filepath.Join(mountpoint, m)) {
			return false
		}
	}
	return true
}

func detectDistro(mountpoint string) string {
	data, err := os.ReadFile(filepath.Join(mountpoint, "etc/os-release"))
	if err != nil {
		return "unknown"
	}
	content := strings.ToLower(string(data))

	distros := []struct {
		keyword string
		name    string
	}{
		{"fedora", "fedora"},
		{"red hat", "rhel"},
		{"centos", "centos"},
		{"rocky", "rocky"},
		{"alma", "alma"},
		{"debian", "debian"},
		{"ubuntu", "ubuntu"},
		{"mint", "linuxmint"},
		{"arch", "arch"},
		{"manjaro", "manjaro"},
		{"opensuse", "opensuse"},
		{"suse", "suse"},
	}

	for _, d := range distros {
		if strings.Contains(content, d.keyword) {
			return d.name
		}
	}

	return "unknown"
}

// parseFstab reads /etc/fstab and returns a map of mountpoint ??device.
// Resolves UUID= and LABEL= references.
func parseFstab(path string) map[string]string {
	result := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return result
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		dev := fields[0]
		mp := fields[1]

		// Skip swap, proc, sysfs, etc.
		if mp == "none" || mp == "swap" || !strings.HasPrefix(mp, "/") {
			continue
		}
		if strings.HasPrefix(dev, "#") {
			continue
		}

		// Resolve UUID= and LABEL=
		resolved := resolveDevice(dev)
		if resolved != "" {
			result[mp] = resolved
		}
	}

	return result
}

func resolveDevice(dev string) string {
	if strings.HasPrefix(dev, "UUID=") {
		uuid := strings.TrimPrefix(dev, "UUID=")
		link := "/dev/disk/by-uuid/" + uuid
		target, err := filepath.EvalSymlinks(link)
		if err == nil {
			return target
		}
		// Try blkid fallback
		out, err := exec.Command("blkid", "-U", uuid).Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
		return "" // UUID not found on this system
	}
	if strings.HasPrefix(dev, "LABEL=") {
		label := strings.TrimPrefix(dev, "LABEL=")
		link := "/dev/disk/by-label/" + label
		target, err := filepath.EvalSymlinks(link)
		if err == nil {
			return target
		}
		out, err := exec.Command("blkid", "-L", label).Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
		return ""
	}
	if strings.HasPrefix(dev, "/dev/") {
		return dev
	}
	return ""
}

func findEFIPartNum(disk string, fstabMounts map[string]string) string {
	efiDev, ok := fstabMounts["/boot/efi"]
	if !ok {
		return ""
	}
	// Extract partition number from device path
	// e.g. /dev/sda1 ??1, /dev/nvme0n1p1 ??1
	efiDev = strings.TrimPrefix(efiDev, disk)
	efiDev = strings.TrimPrefix(efiDev, "p") // for NVMe
	if efiDev != "" {
		return efiDev
	}
	return "1" // default to partition 1
}

func findShimPath(mountpoint string) string {
	// Common locations for the EFI shim bootloader
	candidates := []string{
		"EFI/fedora/shimx64.efi",
		"EFI/centos/shimx64.efi",
		"EFI/redhat/shimx64.efi",
		"EFI/rocky/shimx64.efi",
		"EFI/almalinux/shimx64.efi",
		"EFI/BOOT/BOOTX64.EFI",
		"EFI/ubuntu/shimx64.efi",
		"EFI/debian/shimx64.efi",
	}

	efiBase := filepath.Join(mountpoint, "boot/efi")
	for _, c := range candidates {
		full := filepath.Join(efiBase, c)
		if fileExists(full) {
			// Return in EFI path format (backslashes)
			return "\\" + strings.ReplaceAll(c, "/", "\\")
		}
	}
	return ""
}

// ─── System commands ───────────────────────────────────────────────

// ensurePartitionNodes creates /dev device nodes for partitions
// when udev/mdev haven't done it automatically (common on minimal Alpine RAM OS).
func ensurePartitionNodes(disk string) {
	diskBase := filepath.Base(disk) // e.g. "sda"
	sysPath := "/sys/block/" + diskBase

	entries, err := os.ReadDir(sysPath)
	if err != nil {
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		// Partition directories look like "sda1", "sda2", "nvme0n1p1", etc.
		if !strings.HasPrefix(name, diskBase) {
			continue
		}
		if name == diskBase {
			continue // skip the disk itself
		}

		devPath := filepath.Join(sysPath, name, "dev")
		data, err := os.ReadFile(devPath)
		if err != nil {
			continue
		}

		// Format: "8:1" (major:minor)
		parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
		if len(parts) != 2 {
			continue
		}

		nodePath := "/dev/" + name
		if _, err := os.Stat(nodePath); err == nil {
			continue // already exists
		}

		runQuiet("mknod", nodePath, "b", parts[0], parts[1])
	}
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runQuiet(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func mount(dev, target string) error {
	os.MkdirAll(target, 0755)
	return runQuiet("mount", dev, target)
}

func mountWithType(dev, target, fstype string) error {
	os.MkdirAll(target, 0755)
	return runQuiet("mount", "-t", fstype, dev, target)
}

func mountBind(src, target string) error {
	return runQuiet("mount", "--bind", src, target)
}

func mountTmpfs(target string) error {
	return runQuiet("mount", "-t", "tmpfs", "tmpfs", target)
}

func umountSingle(target string) {
	runQuiet("umount", target)
}

// umountAll unmounts everything under mountRoot in reverse order.
func umountAll() {
	// Read /proc/mounts to find all mounts under mountRoot
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		// Fallback: try umount -R
		runQuiet("umount", "-R", mountRoot)
		return
	}

	// Collect mount points under mountRoot, sorted by depth (deepest first)
	var mounts []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		mp := fields[1]
		if strings.HasPrefix(mp, mountRoot) {
			mounts = append(mounts, mp)
		}
	}

	// Sort by length descending (deepest paths first)
	for i := 0; i < len(mounts); i++ {
		for j := i + 1; j < len(mounts); j++ {
			if len(mounts[j]) > len(mounts[i]) {
				mounts[i], mounts[j] = mounts[j], mounts[i]
			}
		}
	}

	for _, mp := range mounts {
		runQuiet("umount", "-l", mp)
	}
}

func chrootExec(root string, name string, args ...string) error {
	fullArgs := append([]string{root, name}, args...)
	cmd := exec.Command("chroot", fullArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	// Set PATH inside chroot so commands are found
	cmd.Env = []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=linux",
	}

	return cmd.Run()
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// ─── Fstab fix ──────────────────────────────────────────────────────

// fixFstab removes or softens fstab entries for extra disks that won't
// exist after cloning to new hardware. This prevents systemd from
// waiting 90s for non-existent devices and entering emergency mode.
//
// Strategy (B+C):
//
//	/        ??keep as-is (root)
//	/boot*   ??keep as-is (needed for kernel updates)
//	swap     ??keep as-is
//	/mnt/*   ??comment out (external data disks)
//	/data*   ??comment out
//	/media/* ??comment out
//	/backup  ??comment out
//	other    ??add "nofail,x-systemd.device-timeout=10s"
func fixFstab(rootMount string) error {
	fstabPath := filepath.Join(rootMount, "etc/fstab")
	bakPath := filepath.Join(rootMount, "etc/fstab.bak")

	// Back up the original first
	if err := copyFile(fstabPath, bakPath); err != nil {
		// Non-fatal: continue with the fix even if backup fails
	}

	data, err := os.ReadFile(fstabPath)
	if err != nil {
		return fmt.Errorf("read fstab: %w", err)
	}

	extraMounts := map[string]bool{
		"/mnt":     true,
		"/data":    true,
		"/backup":  true,
		"/media":   true,
		"/srv":     true,
		"/var/lib/docker": true,
	}

	keepMounts := map[string]bool{
		"/":         true,
		"/boot":     true,
		"/boot/efi": true,
	}

	lines := strings.Split(string(data), "\n")
	var out []string

	for _, line := range lines {
		raw := line
		line = strings.TrimSpace(line)

		// Pass through comments and blanks
		if line == "" || strings.HasPrefix(line, "#") {
			out = append(out, raw)
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			out = append(out, raw)
			continue
		}

		mp := fields[1]

		// Skip non-path entries (swap, bind mounts, proc, etc.)
		if !strings.HasPrefix(mp, "/") {
			out = append(out, raw)
			continue
		}

		// Protect root and boot
		if keepMounts[mp] || (mp != "/" && strings.HasPrefix(mp, "/boot")) {
			out = append(out, raw)
			continue
		}

		// Comment out known extra-disk mount points (with prefix matching)
		commented := false
		for prefix := range extraMounts {
			if mp == prefix || strings.HasPrefix(mp, prefix+"/") {
				out = append(out, "# "+raw+"  # disabled by disk-cloner: extra disk mount")
				commented = true
				break
			}
		}
		if commented {
			continue
		}

		// All other mount points: add nofail so missing devices don't block boot
		// Also set a short device timeout
		if len(fields) >= 4 {
			opts := fields[3]
			if !strings.Contains(opts, "nofail") {
				fields[3] = opts + ",nofail,x-systemd.device-timeout=10s"
			}
		} else if len(fields) == 2 {
			fields = append(fields, "defaults", "nofail,x-systemd.device-timeout=10s", "0", "0")
		} else if len(fields) == 3 {
			fields = append(fields, "nofail,x-systemd.device-timeout=10s", "0", "0")
		}
		out = append(out, strings.Join(fields, " "))
	}

	return os.WriteFile(fstabPath, []byte(strings.Join(out, "\n")+"\n"), 0644)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
