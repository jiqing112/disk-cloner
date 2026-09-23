package fixboot

import (
	"bufio"
	"errors"
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
	log("刷新分区表...")
	run("partprobe", cfg.TargetDisk)
	run("blockdev", "--rereadpt", cfg.TargetDisk)
	time.Sleep(2 * time.Second)

	// Create device nodes — Alpine uses mdev instead of udev,
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
		log("检测 LVM...")
		// Route through the lvm wrapper: Alpine's lvm2 package provides the
		// standalone vgscan/vgchange symlinks on some builds only.
		run("lvm", "vgscan", "--mknodes")
		run("lvm", "vgchange", "-ay")
		time.Sleep(time.Second)
	} else {
		log("未检测到 lvm 工具 (如需 LVM 支持: apk add lvm2)")
	}

	// ── 3. Find root filesystem ────────────────────────────────────
	log("查找根文件系统...")
	rootDev, rootFstype, err := findRootPartition(cfg.TargetDisk)
	if err != nil {
		return fmt.Errorf("找不到根文件系统: %w\n  请手动指定分区并挂载到 %s", err, mountRoot)
	}
	log("  根分区: %s (%s)", rootDev, rootFstype)

	// Mount root
	if err := mountWithType(rootDev, mountRoot, rootFstype); err != nil {
		// Fallback to auto-detect
		if err2 := mount(rootDev, mountRoot); err2 != nil {
			return fmt.Errorf("挂载根分区 %s (fstype=%s): %v; 自动探测同样失败: %v", rootDev, rootFstype, err, err2)
		}
	}
	defer func() {
		// Cleanup on error paths must not overwrite Run's return value,
		// but a leftover mount can mean an unflushed journal — warn loudly.
		if uerr := umountAll(); uerr != nil {
			log("  [!] 清理阶段卸载失败: %v — 可能存在残留挂载, 请手动 umount 后再重启", uerr)
		}
	}()

	// ── 4. Detect distro ───────────────────────────────────────────
	distro := detectDistro(mountRoot)
	log("  系统类型: %s", distro)

	// ── 5. Parse fstab and mount /boot, /boot/efi ──────────────────
	fstabMounts := parseFstab(filepath.Join(mountRoot, "etc/fstab"))

	if dev, ok := fstabMounts["/boot"]; ok {
		bootDir := filepath.Join(mountRoot, "boot")
		log("  挂载 /boot: %s", dev)
		if err := mount(dev, bootDir); err != nil {
			// Rebuilding initramfs with the real /boot unmounted writes into
			// a shadowed directory on the root partition — the result is
			// invisible after boot and the repair silently does nothing.
			return fmt.Errorf("挂载独立 /boot 分区 (%s) 失败: %w — 无法安全重建 initramfs, 已中止", dev, err)
		}
	}

	espMountedDev := ""
	if dev, ok := fstabMounts["/boot/efi"]; ok {
		efiDir := filepath.Join(mountRoot, "boot/efi")
		os.MkdirAll(efiDir, 0755)
		log("  挂载 /boot/efi: %s", dev)
		if err := mount(dev, efiDir); err != nil {
			log("  [!] 挂载 /boot/efi 失败: %v (UEFI 模式下 GRUB 安装将失败)", err)
		} else {
			espMountedDev = dev
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
	if err := mountTmpfs(runDir); err != nil {
		log("  [!] /run tmpfs 挂载失败: %v (继续; chroot 内部分工具可能行为异常)", err)
	}

	// ── 7. Rebuild initramfs ───────────────────────────────────────
	log("重建 initramfs (包含所有硬件驱动)...")
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
			return fmt.Errorf("未找到 initramfs 重建工具 (dracut/update-initramfs/mkinitcpio)")
		}
	}
	if err != nil {
		return fmt.Errorf("重建 initramfs 失败: %w", err)
	}
	log("  ✓ initramfs 重建完成")

	// ── 8. Reinstall GRUB ──────────────────────────────────────────
	log("修复 GRUB 引导...")
	// Boot mode follows the machine's own firmware — this tool runs on the
	// box that will boot the disk, and /sys/firmware/efi exists only on
	// UEFI boots. The firmware verdict is absolute: fstab/ESP hints must
	// not be able to flip it, or a BIOS box restoring a UEFI-origin image
	// gets an unbootable UEFI install reported as success.
	fwInfo, fwErr := os.Stat("/sys/firmware/efi")
	fwEFI := fwErr == nil && fwInfo.IsDir()
	_, efiInFstab := fstabMounts["/boot/efi"]

	// Locate the ESP. Default /boot/efi; some systems mount the ESP at
	// /boot directly (vfat fstab entry + EFI/ directory).
	efiSub := "/boot/efi"
	espDev := espMountedDev
	if espDev == "" {
		if bdev, ok := fstabMounts["/boot"]; ok && getBlkidFstype(bdev) == "vfat" &&
			dirExists(filepath.Join(mountRoot, "boot/EFI")) {
			espDev = bdev
			efiSub = "/boot"
		}
	}
	// Firmware says UEFI but no ESP found via fstab: scan the target disk's
	// vfat partitions for one carrying an EFI/ directory (covers ESPs
	// referenced only by PARTUUID or systemd's /efi automount).
	if fwEFI && espDev == "" && !dirExists(filepath.Join(mountRoot, efiSub, "EFI")) {
		if dev, ok := scanAndMountESP(cfg.TargetDisk); ok {
			espDev = dev
		}
	}

	isEFI := decideEFI(fwErr, fwEFI, efiInFstab, espDev,
		dirExists(filepath.Join(mountRoot, efiSub, "EFI")))

	// Track failed steps so Run can report them instead of printing a
	// misleading success line.
	grubFailed := false
	mkcfgFailed := false
	grubToolFound := false

	// grub.cfg regeneration is tracked separately: a stale grub.cfg (old
	// UUIDs) boots as badly as a failed install.
	runMkconfig := func(tool, cfgFile string) {
		if err := chrootExec(mountRoot, tool, "-o", cfgFile); err != nil {
			mkcfgFailed = true
			log("  [!] %s 失败: %v", tool, err)
		}
	}

	// bootID names the NVRAM boot entry; match the distro's EFI directory
	// so it lines up with the shim path.
	bootID := "Linux"
	switch distro {
	case "fedora", "rhel", "centos", "rocky", "alma", "opensuse", "suse",
		"arch", "manjaro", "debian", "ubuntu", "linuxmint":
		bootID = distro
	}

	if isEFI {
		log("  检测到 UEFI 模式 (固件: %v, ESP: %s)", fwEFI, efiSub)
		// Fedora/RHEL
		if fileExists(filepath.Join(mountRoot, "usr/sbin/grub2-install")) {
			grubToolFound = true
			err = chrootExec(mountRoot, "grub2-install",
				"--target=x86_64-efi",
				"--efi-directory="+efiSub,
				"--bootloader-id="+bootID,
				"--recheck")
			if err != nil {
				grubFailed = true
				log("  [!] grub2-install 失败: %v (可能需要手动处理)", err)
			}
			if fileExists(filepath.Join(mountRoot, "usr/sbin/grub2-mkconfig")) {
				runMkconfig("grub2-mkconfig", "/boot/grub2/grub.cfg")
			}
		} else if fileExists(filepath.Join(mountRoot, "usr/sbin/grub-install")) {
			grubToolFound = true
			err = chrootExec(mountRoot, "grub-install",
				"--target=x86_64-efi",
				"--efi-directory="+efiSub,
				"--recheck")
			if err != nil {
				grubFailed = true
				log("  [!] grub-install 失败: %v", err)
			}
			if fileExists(filepath.Join(mountRoot, "usr/sbin/grub-mkconfig")) {
				runMkconfig("grub-mkconfig", "/boot/grub/grub.cfg")
			}
		}

		// Add UEFI boot entry if efibootmgr is available
		if commandExists("efibootmgr") {
			// efibootmgr needs efivarfs, which the minimal Alpine RAM OS
			// rarely mounts on its own. Already mounted or failure: both
			// fine — the efibootmgr call below reports if it still can't
			// work.
			runQuiet("mount", "-t", "efivarfs", "efivarfs", "/sys/firmware/efi/efivars")
			if espDev != "" {
				// The ESP may live on a different disk than the clone
				// target — efibootmgr must reference the disk that
				// actually hosts it.
				efiDisk, efiPart := findEFIPart(espDev)
				if efiDisk == "" {
					efiDisk = cfg.TargetDisk
				}
				if efiPart == "" {
					efiPart = "1"
				}
				shimPath := findShimPath(mountRoot, efiSub)
				if shimPath != "" {
					if err := run("efibootmgr", "-c",
						"-d", efiDisk,
						"-p", efiPart,
						"-L", bootID,
						"-l", shimPath); err != nil {
						log("  [!] efibootmgr 添加启动项失败: %v — NVRAM 启动项未写入; grub-install 通常已写 fallback 路径 \\EFI\\BOOT\\BOOTX64.EFI, 多数固件仍可引导, 否则需手动添加启动项", err)
					} else {
						log("  ✓ UEFI 引导项已添加 (%s 分区 %s)", efiDisk, efiPart)
					}
				}
			}
		} else {
			log("efibootmgr not found, UEFI boot entry may need manual setup")
			log("    Install on Alpine: apk add efibootmgr")
		}
	} else {
		log("  检测到 BIOS/Legacy 模式")
		if fileExists(filepath.Join(mountRoot, "usr/sbin/grub2-install")) {
			grubToolFound = true
			err = chrootExec(mountRoot, "grub2-install", "--recheck", cfg.TargetDisk)
			if err != nil {
				grubFailed = true
				log("  [!] grub2-install 失败: %v", err)
			}
			if fileExists(filepath.Join(mountRoot, "usr/sbin/grub2-mkconfig")) {
				runMkconfig("grub2-mkconfig", "/boot/grub2/grub.cfg")
			}
		} else if fileExists(filepath.Join(mountRoot, "usr/sbin/grub-install")) {
			grubToolFound = true
			err = chrootExec(mountRoot, "grub-install", "--recheck", cfg.TargetDisk)
			if err != nil {
				grubFailed = true
				log("  [!] grub-install 失败: %v", err)
			}
			if fileExists(filepath.Join(mountRoot, "usr/sbin/grub-mkconfig")) {
				runMkconfig("grub-mkconfig", "/boot/grub/grub.cfg")
			}
		}
	}
	if grubFailed {
		log("  ✗ GRUB 重装失败")
	} else if mkcfgFailed {
		log("  ✗ grub.cfg 生成失败")
	} else if grubToolFound {
		log("  ✓ GRUB 修复完成")
	} else {
		log("  [!] 未找到 GRUB 安装工具 (grub2-install/grub-install), 跳过 GRUB 重装")
	}

	// ── 9. Fix fstab: remove extra disk mounts ─────────────────────
	log("修复 fstab (移除不存在的额外磁盘挂载)...")
	if err := fixFstab(mountRoot); err != nil {
		log("  ✗ fstab 修复失败: %v (不影响启动, 可手动处理)", err)
	}

	// ── 10. Cleanup ─────────────────────────────────────────────────
	log("清理挂载点...")
	if uerr := umountAll(); uerr != nil {
		return fmt.Errorf("部分挂载点无法卸载: %w — 请手动 umount 后再重启, 否则文件系统日志可能未完整落盘", uerr)
	}
	if grubFailed {
		if isEFI {
			return fmt.Errorf("GRUB 重装失败 (UEFI), 目标盘可能无法启动 (initramfs 已重建); 请手动执行 grub-install --target=x86_64-efi --efi-directory=%s", efiSub)
		}
		return fmt.Errorf("GRUB 重装失败, 目标盘可能无法启动 (initramfs 已重建); 请手动执行 grub-install --recheck %s", cfg.TargetDisk)
	}
	if mkcfgFailed {
		return fmt.Errorf("grub.cfg 生成失败 (GRUB 已重装, 目标盘可能仍引导异常); 请进入系统后手动执行 grub-mkconfig -o 对应 grub.cfg 路径")
	}
	log("✓ 引导修复完成!")

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

	// 2. LVM logical volumes — only those physically located on the target
	// disk (/sys/block/dm-N/slaves names the devices a dm volume is built
	// on). /dev/mapper/* spans every VG on the box; without the filter a
	// multi-disk machine could pick another disk's root by alphabetical
	// accident.
	lvmDevs, _ := filepath.Glob("/dev/mapper/*")
	for _, d := range lvmDevs {
		if d != "/dev/mapper/control" && dmOnTargetDisk(d, targetDisk) {
			candidates = append(candidates, d)
		}
	}
	dmDevs, _ := filepath.Glob("/dev/dm-*")
	for _, d := range dmDevs {
		if dmOnTargetDisk(d, targetDisk) {
			candidates = append(candidates, d)
		}
	}

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

// dmOnTargetDisk reports whether the device-mapper device devPath (a
// /dev/dm-N node, or a /dev/mapper/* name that resolves to one) is built on
// targetDisk or one of its partitions: every entry of
// /sys/block/dm-N/slaves is a kernel basename like sda3 or nvme0n1p1.
func dmOnTargetDisk(devPath, targetDisk string) bool {
	real, err := filepath.EvalSymlinks(devPath)
	if err != nil {
		real = devPath
	}
	data, err := os.ReadFile("/sys/block/" + filepath.Base(real) + "/slaves")
	if err != nil {
		// Not a dm device or sysfs unreadable — excluding is the safe side:
		// the plain-partition candidates above still cover the target disk.
		return false
	}
	diskBase := filepath.Base(targetDisk)
	for _, s := range strings.Fields(string(data)) {
		if slaveMatchesDisk(s, diskBase) {
			return true
		}
	}
	return false
}

// slaveMatchesDisk reports whether slave (a kernel basename from a dm
// device's slaves list, e.g. "sda3" or "nvme0n1p1") is diskBase itself or
// one of its partitions. Disk names ending in a digit (nvme0n1, mmcblk0,
// loop0) separate partition numbers with a "p"; a bare prefix check would
// wrongly count nvme0n10 — a different disk — as a partition of nvme0n1.
func slaveMatchesDisk(slave, diskBase string) bool {
	if slave == diskBase {
		return true
	}
	rest := strings.TrimPrefix(slave, diskBase)
	if len(rest) == len(slave) {
		return false // slave does not even start with diskBase
	}
	if c := diskBase[len(diskBase)-1]; c >= '0' && c <= '9' {
		return strings.HasPrefix(rest, "p") && allDigits(rest[1:])
	}
	return allDigits(rest)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// decideEFI resolves the boot mode from the /sys/firmware/efi probe.
// Firmware is authoritative: a completed probe decides alone, and plain
// absence of the directory is exactly what a BIOS boot looks like. Only an
// abnormal probe error (something other than NotExist, i.e. /sys in an
// unexpected state) falls back to the fstab/ESP heuristics.
func decideEFI(probeErr error, fwIsDir, fstabEFI bool, espDev string, espDirPresent bool) bool {
	if probeErr == nil {
		return fwIsDir
	}
	// errors.Is (not os.IsNotExist): it unwraps %w-wrapped errors, which
	// callers may pass when testing.
	if errors.Is(probeErr, os.ErrNotExist) {
		return false
	}
	return fstabEFI || espDev != "" || espDirPresent
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
		// ubuntu/mint must be checked before debian: their os-release
		// contains ID_LIKE=debian, which would otherwise match first.
		{"ubuntu", "ubuntu"},
		{"mint", "linuxmint"},
		{"debian", "debian"},
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

// parseFstab reads /etc/fstab and returns a map of mountpoint → device.
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

// resolveDevice maps an fstab source column to a /dev node. fstab keywords
// are case-insensitive ("uuid=" is legal); the referenced value is not.
// PARTUUID=/PARTLABEL= are handled alongside UUID=/LABEL= — Raspberry Pi OS,
// Armbian and systemd-gpt-auto installs reference their /boot and ESP by
// PARTUUID, and skipping those entries left /boot unmounted and misjudged
// UEFI systems as BIOS.
func resolveDevice(dev string) string {
	upper := strings.ToUpper(dev)
	switch {
	case strings.HasPrefix(upper, "UUID="):
		uuid := dev[len("UUID="):]
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
	case strings.HasPrefix(upper, "PARTUUID="):
		id := dev[len("PARTUUID="):]
		target, err := filepath.EvalSymlinks("/dev/disk/by-partuuid/" + id)
		if err == nil {
			return target
		}
		out, err := exec.Command("blkid", "-t", "PARTUUID="+id, "-o", "device").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case strings.HasPrefix(upper, "LABEL="):
		label := dev[len("LABEL="):]
		link := "/dev/disk/by-label/" + label
		target, err := filepath.EvalSymlinks(link)
		if err == nil {
			return target
		}
		out, err := exec.Command("blkid", "-L", label).Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case strings.HasPrefix(upper, "PARTLABEL="):
		id := dev[len("PARTLABEL="):]
		target, err := filepath.EvalSymlinks("/dev/disk/by-partlabel/" + id)
		if err == nil {
			return target
		}
		out, err := exec.Command("blkid", "-t", "PARTLABEL="+id, "-o", "device").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case strings.HasPrefix(dev, "/dev/"):
		return dev
	}
	return ""
}

// scanAndMountESP probes the target disk's vfat partitions for an EFI
// System Partition (vfat + EFI/ directory) and mounts the first hit at
// mountRoot/boot/efi. Used when the firmware booted UEFI but fstab yields
// no usable /boot/efi entry (PARTUUID-only systems, /efi automounts, ...).
func scanAndMountESP(targetDisk string) (string, bool) {
	if dirExists(filepath.Join(mountRoot, "boot/efi/EFI")) {
		return "", true // an ESP is already mounted there
	}
	probeDir := mountRoot + ".probe"
	entries, _ := filepath.Glob(targetDisk + "*")
	for _, e := range entries {
		if e == targetDisk || getBlkidFstype(e) != "vfat" {
			continue
		}
		if err := mount(e, probeDir); err != nil {
			continue
		}
		hasEFI := dirExists(filepath.Join(probeDir, "EFI"))
		umountSingle(probeDir)
		if hasEFI {
			if err := mount(e, filepath.Join(mountRoot, "boot/efi")); err == nil {
				return e, true
			}
			return "", false
		}
	}
	return "", false
}

// findEFIPart splits an EFI system partition device (e.g. /dev/sda1 or
// /dev/nvme0n1p1) into its disk and partition number. The ESP may live on a
// different disk than the clone target, so efibootmgr must reference the
// disk that actually hosts it.
func findEFIPart(efiDev string) (string, string) {
	i := len(efiDev)
	for i > 0 && efiDev[i-1] >= '0' && efiDev[i-1] <= '9' {
		i--
	}
	part := efiDev[i:]
	disk := efiDev[:i]
	// nvme0n1p1 / mmcblk0p1 style: the trailing "p" is a separator, not
	// part of the disk name (only strip it when the rest carries digits).
	if strings.HasSuffix(disk, "p") {
		base := disk[:len(disk)-1]
		if strings.ContainsAny(filepath.Base(base), "0123456789") {
			disk = base
		}
	}
	return disk, part
}

func findShimPath(mountpoint, efiSub string) string {
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

	efiBase := filepath.Join(mountpoint, strings.TrimPrefix(efiSub, "/"))
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
	// Retry regular umount; do NOT use lazy umount (-l) because it leaves
	// the filesystem live in the kernel and the journal mid-transaction,
	// which corrupts subsequent dd reads.
	for try := 0; try < 5; try++ {
		if runQuiet("umount", target) == nil {
			return
		}
		runQuiet("sync")
		time.Sleep(time.Second)
	}
}

// umountAll unmounts everything under mountRoot in reverse order.
// Uses regular umount with retries (NOT lazy umount) so that the filesystem
// journal is properly committed to disk before any subsequent dd operation.
// Returns an error listing mount points that could not be unmounted.
func umountAll() error {
	// Read /proc/mounts to find all mounts under mountRoot
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		// Fallback: umount -R is all that's left without /proc/mounts; a
		// failure here can mean a live journal — surface it, don't fake
		// success.
		if rerr := runQuiet("umount", "-R", mountRoot); rerr != nil {
			return fmt.Errorf("无法读取 /proc/mounts, umount -R %s 亦失败: %v", mountRoot, rerr)
		}
		return nil
	}

	// Collect mount points under mountRoot, sorted by depth (deepest first)
	var mounts []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		mp := fields[1]
		if mp == mountRoot || strings.HasPrefix(mp, mountRoot+"/") {
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

	// Try up to 5 rounds of regular umount. After each round re-read
	// /proc/mounts to skip already-unmounted entries.
	for try := 0; try < 5; try++ {
		remaining := mounts[:0]
		for _, mp := range mounts {
			if runQuiet("umount", mp) != nil {
				remaining = append(remaining, mp)
			}
		}
		mounts = remaining
		if len(mounts) == 0 {
			break
		}
		runQuiet("sync")
		time.Sleep(time.Second)
	}

	// Never lazy-unmount (-l): it detaches the mount but leaves the
	// filesystem live in the kernel with a mid-transaction journal — the
	// exact corruption this tool exists to avoid (see umountSingle).
	// Report the leftovers so the caller can refuse to proceed.
	if len(mounts) > 0 {
		return fmt.Errorf("卸载失败: %s", strings.Join(mounts, ", "))
	}
	return nil
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
//	/        → keep as-is (root)
//	/boot*   → keep as-is (needed for kernel updates)
//	swap     → keep as-is
//	/mnt/*   → comment out (external data disks)
//	/data*   → comment out
//	/media/* → comment out
//	/backup  → comment out
//	other    → add "nofail,x-systemd.device-timeout=10s"
func fixFstab(rootMount string) error {
	fstabPath := filepath.Join(rootMount, "etc/fstab")
	bakPath := filepath.Join(rootMount, "etc/fstab.bak")

	// Back up the original first — never overwrite an existing backup, so
	// a second run keeps the true original rather than a copy of the
	// already-modified file.
	if fileExists(bakPath) {
		fmt.Printf("  [!] %s 已存在, 保留最早的原始备份\n", bakPath)
	} else if err := copyFile(fstabPath, bakPath); err != nil {
		// Non-fatal: continue with the fix even if backup fails
		fmt.Printf("  [!] fstab 备份失败: %v (继续修复)\n", err)
	}

	data, err := os.ReadFile(fstabPath)
	if err != nil {
		return fmt.Errorf("read fstab: %w", err)
	}

	extraMounts := map[string]bool{
		"/mnt":            true,
		"/data":           true,
		"/backup":         true,
		"/media":          true,
		"/srv":            true,
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
			// Malformed short line: fstype column must exist, otherwise
			// mount fails with "unknown filesystem type defaults".
			fields = append(fields, "auto", "defaults,nofail,x-systemd.device-timeout=10s", "0", "0")
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
