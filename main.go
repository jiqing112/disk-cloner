package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"disk-cloner/internal/cli"
	"disk-cloner/internal/clone"
	"disk-cloner/internal/disk"
	"disk-cloner/internal/fixboot"
	sshclient "disk-cloner/internal/ssh"
)

func main() {
	var (
		remoteIP   = flag.String("H", "", "Remote server IP")
		remotePort = flag.Int("P", 22, "Remote SSH port")
		remoteUser = flag.String("u", "root", "Remote SSH user")
		remotePass = flag.String("p", "", "Remote SSH password")
		source     = flag.String("s", "", "Source disk (remote, e.g. /dev/sda)")
		target     = flag.String("t", "", "Target disk (local, e.g. /dev/sda)")
		bs         = flag.String("bs", "4M", "Block size")
		autoYes    = flag.Bool("y", false, "Skip confirmation")
		saveFile   = flag.String("o", "", "Save as gzip file instead of cloning to disk")
		noFixBoot  = flag.Bool("no-fix-boot", false, "Skip automatic boot repair after clone")
		fixBootDev = flag.String("fix-boot-disk", "", "Standalone: fix boot on a disk (e.g. /dev/sda)")
	)
	flag.Parse()

	// Auto-install required packages on Alpine Linux
	ensureDeps()

	// Standalone fix-boot mode
	if *fixBootDev != "" {
		fmt.Println()
		fmt.Println("  ═══════════════════════════════════════════")
		fmt.Println("  修复引导 - 独立模式")
		fmt.Println("  ═══════════════════════════════════════════")
		fmt.Println()
		if err := fixboot.Run(fixboot.Config{TargetDisk: *fixBootDev}); err != nil {
			fmt.Printf("\n  ✗ 修复失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Non-interactive mode
	if *remoteIP != "" && *source != "" && (*target != "" || *saveFile != "") {
		runDirect(*remoteIP, *remotePort, *remoteUser, *remotePass,
			*source, *target, *bs, *autoYes, *saveFile, *noFixBoot)
		return
	}

	// Interactive mode
	runInteractive()
}

const remoteLsblkCmd = "lsblk -Jb -o NAME,SIZE,TYPE,MOUNTPOINT,MODEL,SERIAL,TRAN,ROTA,RM,FSTYPE,LABEL"

// ensureDeps checks and installs required packages on Alpine Linux.
// Does nothing if not on Alpine or packages are already installed.
func ensureDeps() {
	// Only run if apk is available (Alpine Linux)
	if _, err := exec.LookPath("apk"); err != nil {
		return
	}

	// Packages needed and the binary they provide (to check if installed)
	deps := []struct {
		pkg    string
		binary string
	}{
		{"util-linux", "lsblk"},
		{"lvm2", "lvm"},
		{"e2fsprogs", "mkfs.ext4"},   // ext4 mount support
		{"xfsprogs", "mkfs.xfs"},     // xfs mount support
		{"btrfs-progs", "mkfs.btrfs"},// btrfs mount support
		{"efibootmgr", "efibootmgr"},
	}

	var missing []string
	for _, d := range deps {
		if _, err := exec.LookPath(d.binary); err != nil {
			missing = append(missing, d.pkg)
		}
	}

	if len(missing) == 0 {
		return
	}

	fmt.Printf("  正在安装依赖: %s ...\n", strings.Join(missing, ", "))
	args := append([]string{"add", "--quiet"}, missing...)
	cmd := exec.Command("apk", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("  ⚠ 部分依赖安装失败 (继续运行): %v\n", err)
	} else {
		fmt.Println("  ✓ 依赖安装完成")
	}
	fmt.Println()
}

// ensureRemoteDeps installs required packages on the remote Alpine Linux via SSH.
func ensureRemoteDeps(sshClient *sshclient.Client) {
	// Check if remote has apk (Alpine Linux)
	if _, err := sshClient.CombinedOutput("command -v apk"); err != nil {
		return // not Alpine, skip
	}

	// Check which tools are missing
	checks := []struct {
		cmd string
		pkg string
	}{
		{"lsblk", "util-linux"},
		{"gzip", "gzip"},
	}

	var missing []string
	for _, c := range checks {
		if _, err := sshClient.CombinedOutput("command -v " + c.cmd); err != nil {
			missing = append(missing, c.pkg)
		}
	}

	if len(missing) == 0 {
		return
	}

	fmt.Printf("  正在安装远程依赖: %s ...\n", strings.Join(missing, ", "))
	installCmd := "apk add --quiet " + strings.Join(missing, " ")
	out, err := sshClient.CombinedOutput(installCmd)
	if err != nil {
		fmt.Printf("  ⚠ 远程依赖安装失败: %v %s\n", err, out)
	} else {
		fmt.Println("  ✓ 远程依赖安装完成")
	}
}

// formatTotalTime formats a duration into a human-readable string.
func formatTotalTime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1f 秒", d.Seconds())
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%d 分 %d 秒", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%d 小时 %d 分 %d 秒", h, m, s)
}

func runInteractive() {
	cli.PrintHeader()

	// ── SSH Config ─────────────────────────────────────────────────
	fmt.Println("  远程服务器配置")
	fmt.Println("  ─────────────────────────────────────────────")
	ip := cli.ReadInput("服务器IP", "")
	if ip == "" {
		fmt.Println("  取消")
		return
	}
	port := cli.ReadInt("SSH 端口", 22)
	user := cli.ReadInput("用户名", "root")
	pass := cli.ReadPassword("SSH 密码 (回车使用密钥)")
	if pass == "" {
		fmt.Println("  将尝试使用 SSH 密钥认证...")
	}

	// ── Connect ────────────────────────────────────────────────────
	fmt.Println()
	fmt.Print("  正在连接...")

	sshClient, err := sshclient.Connect(sshclient.Config{
		Host: ip, Port: port, User: user, Password: pass, Timeout: 15,
	})
	if err != nil {
		fmt.Printf("\r\033[K  ✗ 连接失败: %v\n", err)
		os.Exit(1)
	}
	defer sshClient.Close()
	fmt.Printf("\r\033[K  ✓ SSH 连接成功 (%s@%s:%d)\n", user, ip, port)

	// Auto-install deps on remote Alpine
	ensureRemoteDeps(sshClient)

	// ── Scan remote disks ──────────────────────────────────────────
	fmt.Print("  正在扫描远程磁盘...")
	remoteRaw, err := sshClient.CombinedOutput(remoteLsblkCmd)
	if err != nil || remoteRaw == "" {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if remoteRaw != "" {
			msg = remoteRaw
		}
		fmt.Printf("\r\033[K  ✗ 扫描远程磁盘失败: %s\n", msg)
		fmt.Println("     请确认远程已安装 lsblk (apk add util-linux)")
		os.Exit(1)
	}
	remoteDisks, err := disk.ParseJSON(remoteRaw)
	if err != nil {
		fmt.Printf("\r\033[K  ✗ 解析远程磁盘信息失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\r\033[K  ✓ 发现 %d 块远程磁盘\n", countType(remoteDisks, "disk"))

	// ── Scan local disks ───────────────────────────────────────────
	fmt.Print("  正在扫描本地磁盘...")
	localDisks, err := disk.GetLocalDisks()
	if err != nil {
		fmt.Printf("\r\033[K  ✗ 扫描本地磁盘失败: %v\n", err)
		fmt.Println("     请确认本地已安装 lsblk (apk add util-linux)")
		os.Exit(1)
	}
	fmt.Printf("\r\033[K  ✓ 发现 %d 块本地磁盘\n", countType(localDisks, "disk"))

	// ── Build lists ────────────────────────────────────────────────
	remoteList := filterDisks(remoteDisks)
	localList := filterDisks(localDisks)

	if len(remoteList) == 0 {
		fmt.Println("\n  ✗ 远程未发现磁盘设备")
		os.Exit(1)
	}

	cli.PrintSection(fmt.Sprintf("远程磁盘 (%s)", ip))
	cli.PrintDiskList(remoteList, "remote")

	if len(localList) > 0 {
		cli.PrintSection("本地磁盘")
		cli.PrintDiskList(localList, "local")
	}

	// ── Select source ──────────────────────────────────────────────
	fmt.Println()
	srcIdx := cli.SelectDisk("选择源磁盘 (远程)", 1, len(remoteList))
	srcDisk := remoteList[srcIdx-1]

	// ── Select operation mode ──────────────────────────────────────
	fmt.Println()
	fmt.Println("  操作模式:")
	fmt.Println("  [1] 克隆到本地磁盘 (dd → 磁盘)")
	fmt.Println("  [2] 保存为压缩文件 (dd → gzip 文件)")
	mode := cli.SelectOption("选择操作模式", 1, 2)

	if mode == 1 {
		// ── Clone to disk ──────────────────────────────────────────
		if len(localList) == 0 {
			fmt.Println("\n  ✗ 本地未发现磁盘设备")
			os.Exit(1)
		}

		tgtIdx := cli.SelectDisk("选择目标磁盘 (本地)", 1, len(localList))
		tgtDisk := localList[tgtIdx-1]

		fmt.Println()
		fmt.Println("  ┌────────────────────────────────────────────┐")
		fmt.Printf("  │  源:   %s:%s (%s)\n", ip, srcDisk.Path, srcDisk.SizeHuman)
		fmt.Printf("  │  目标: 本地 %s (%s)\n", tgtDisk.Path, tgtDisk.SizeHuman)
		fmt.Println("  └────────────────────────────────────────────┘")

		if tgtDisk.SizeBytes < srcDisk.SizeBytes {
			fmt.Printf("\n  ⚠  警告: 目标盘 (%s) 小于 源盘 (%s)\n",
				tgtDisk.SizeHuman, srcDisk.SizeHuman)
		}

		blockSize := cli.ReadInput("块大小", "4M")

		fmt.Println()
		fmt.Printf("  ⚠  此操作将覆盖 %s 上的所有数据!\n", tgtDisk.Path)
		if !cli.Confirm("  确认开始克隆? 输入 yes 继续") {
			fmt.Println("  已取消")
			return
		}

		fmt.Println()
		fmt.Println("  开始克隆...")
		fmt.Println()

		totalStart := time.Now()

		job := clone.New(sshClient, clone.Params{
			SourcePath: srcDisk.Path,
			TargetPath: tgtDisk.Path,
			SourceSize: srcDisk.SizeBytes,
			BlockSize:  blockSize,
		}, makeProgressFn())
		job.SetLogFunc(func(format string, args ...interface{}) {
			fmt.Printf(format+"\n", args...)
		})

		if err := job.Run(); err != nil {
			fmt.Printf("\n  ✗ 克隆失败: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("  ✓ 克隆完成!")
		fmt.Println()

		// Auto fix-boot — disabled: unreliable on Alpine RAM OS due to
		// missing device nodes, kernel modules, and chroot complexity.
		// Use the manual fstab warning instead.
		_ = fixboot.Run // keep import

		fmt.Println()
		fmt.Println("  ═══════════════════════════════════════════════")
		fmt.Printf("  ✓ 全部完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))
		fmt.Println("  ═══════════════════════════════════════════════")

		printFstabWarning(tgtDisk.Path)

	} else {
		// ── Save to file ───────────────────────────────────────────
		defaultName := makeFileName(ip, srcDisk.Name)
		fileName := cli.ReadInput("保存文件名", defaultName)

		fmt.Println()
		fmt.Println("  ┌────────────────────────────────────────────┐")
		fmt.Printf("  │  源:   %s:%s (%s)\n", ip, srcDisk.Path, srcDisk.SizeHuman)
		fmt.Printf("  │  文件: %s\n", fileName)
		fmt.Println("  │  格式: gzip 压缩")
		fmt.Println("  └────────────────────────────────────────────┘")
		fmt.Println()
		fmt.Println("  ⚠  注意: 如果当前系统在 RAM OS 中运行,")
		fmt.Println("     压缩文件会写入内存文件系统，请确保内存充足")
		fmt.Println("     或将文件保存到已挂载的物理磁盘路径")

		blockSize := cli.ReadInput("块大小", "4M")

		fmt.Println()
		if !cli.Confirm("  确认开始保存? 输入 yes 继续") {
			fmt.Println("  已取消")
			return
		}

		fmt.Println()
		fmt.Println("  开始保存...")
		fmt.Println()

		totalStart := time.Now()

		job := clone.New(sshClient, clone.Params{
			SourcePath: srcDisk.Path,
			TargetPath: fileName,
			SourceSize: srcDisk.SizeBytes,
			BlockSize:  blockSize,
			ZeroFill:   true,
		}, makeProgressFn())
		job.SetLogFunc(func(format string, args ...interface{}) {
			fmt.Printf(format+"\n", args...)
		})

		if err := job.RunToFile(); err != nil {
			fmt.Printf("\n  ✗ 保存失败: %v\n", err)
			os.Exit(1)
		}

		// Show file size
		if info, err := os.Stat(fileName); err == nil {
			ratio := 0.0
			if srcDisk.SizeBytes > 0 {
				ratio = float64(info.Size()) / float64(srcDisk.SizeBytes) * 100
			}
			fmt.Printf("  文件大小: %s (压缩率 %.1f%%)\n", disk.FormatBytes(info.Size()), ratio)
		}
		fmt.Println()
		fmt.Println("  ═══════════════════════════════════════════════")
		fmt.Printf("  ✓ 保存完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))
		fmt.Println("  ═══════════════════════════════════════════════")
	}
}

func runDirect(ip string, port int, user, pass, source, target, bs string,
	autoYes bool, saveFile string, noFixBoot bool) {

	sshClient, err := sshclient.Connect(sshclient.Config{
		Host: ip, Port: port, User: user, Password: pass, Timeout: 30,
	})
	if err != nil {
		log.Fatalf("SSH 连接失败: %v", err)
	}
	defer sshClient.Close()

	// Auto-install deps on remote
	ensureRemoteDeps(sshClient)

	// Get remote disk info
	remoteRaw, err := sshClient.CombinedOutput(remoteLsblkCmd)
	if err != nil || remoteRaw == "" {
		log.Fatalf("扫描远程磁盘失败 (请确认远程已安装 lsblk): %v", err)
	}
	remoteDisks, err := disk.ParseJSON(remoteRaw)
	if err != nil {
		log.Fatalf("解析远程磁盘失败: %v", err)
	}
	srcDisk := disk.FindDisk(remoteDisks, source)
	if srcDisk == nil {
		log.Fatalf("远程磁盘未找到: %s", source)
	}
	fmt.Printf("远程: %s:%s (%s)\n", ip, source, srcDisk.SizeHuman)

	if saveFile != "" {
		// ── Save to file mode ──────────────────────────────────────
		if saveFile == "auto" {
			name := filepath.Base(source)
			saveFile = makeFileName(ip, name)
		}
		fmt.Printf("文件: %s (gzip)\n", saveFile)

		totalStart := time.Now()

		job := clone.New(sshClient, clone.Params{
			SourcePath: source,
			TargetPath: saveFile,
			SourceSize: srcDisk.SizeBytes,
			BlockSize:  bs,
			ZeroFill:   true,
		}, makeProgressFn())
		job.SetLogFunc(func(format string, args ...interface{}) {
			fmt.Printf(format+"\n", args...)
		})

		if err := job.RunToFile(); err != nil {
			fmt.Printf("\n✗ 保存失败: %v\n", err)
			os.Exit(1)
		}

		if info, err := os.Stat(saveFile); err == nil {
			fmt.Printf("文件大小: %s\n", disk.FormatBytes(info.Size()))
		}
		fmt.Printf("保存完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))
		return
	}

	// ── Clone to disk mode ─────────────────────────────────────────
	localDisks, err := disk.GetLocalDisks()
	if err != nil {
		log.Fatalf("扫描本地磁盘失败: %v", err)
	}
	tgtDisk := disk.FindDisk(localDisks, target)
	if tgtDisk == nil {
		log.Fatalf("本地磁盘未找到: %s", target)
	}
	fmt.Printf("本地: %s (%s)\n", target, tgtDisk.SizeHuman)

	if tgtDisk.SizeBytes < srcDisk.SizeBytes {
		fmt.Printf("⚠ 警告: 目标盘 (%s) 小于 源盘 (%s)\n",
			tgtDisk.SizeHuman, srcDisk.SizeHuman)
	}

	if !autoYes {
		fmt.Printf("⚠ 此操作将覆盖 %s 上的所有数据!\n", target)
		fmt.Print("确认继续? (yes/no): ")
		var confirm string
		fmt.Scanln(&confirm)
		if confirm != "yes" && confirm != "y" {
			fmt.Println("已取消")
			return
		}
	}

	fmt.Println("\n开始克隆...")

	totalStart := time.Now()

	job := clone.New(sshClient, clone.Params{
		SourcePath: source,
		TargetPath: target,
		SourceSize: srcDisk.SizeBytes,
		BlockSize:  bs,
	}, makeProgressFn())
	job.SetLogFunc(func(format string, args ...interface{}) {
		fmt.Printf(format+"\n", args...)
	})

	if err := job.Run(); err != nil {
		fmt.Printf("\n✗ 克隆失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("克隆完成!")
	fmt.Printf("总耗时: %s\n", formatTotalTime(time.Since(totalStart)))

	// Auto fix-boot: disabled (see runInteractive for details)
	_ = noFixBoot // keep import usage
	printFstabWarning(target)
}

// printFstabWarning prints a prominent reminder that the user MUST
// mount the cloned disk and fix fstab before rebooting.
// Prints 3 times to ensure it's not missed.
func printFstabWarning(targetDisk string) {
	warn := func() {
		fmt.Println()
		fmt.Println("  ╔══════════════════════════════════════════════╗")
		fmt.Println("  ║  ⚠⚠⚠  重要提醒 - 务必执行后再重启  ⚠⚠⚠  ║")
		fmt.Println("  ╠══════════════════════════════════════════════╣")
		fmt.Println("  ║                                              ║")
		fmt.Println("  ║  克隆后的磁盘可能因 fstab 中额外的磁盘       ║")
		fmt.Println("  ║  挂载配置而导致启动失败 (卡 90 秒超时)。     ║")
		fmt.Println("  ║                                              ║")
		fmt.Printf("  ║  请立即挂载目标磁盘并检查 fstab:            ║\n")
		fmt.Println("  ║                                              ║")
		fmt.Println("  ║  1. 创建分区设备节点:                       ║")
		fmt.Println("  ║     mdev -s                                  ║")
		fmt.Println("  ║                                              ║")
		fmt.Println("  ║  2. 用 lsblk 查看分区号, 挂载根分区:       ║")
		fmt.Printf("  ║     mount %s4 /mnt   (例如最后一个分区)    ║\n", targetDisk)
		fmt.Println("  ║                                              ║")
		fmt.Println("  ║  3. 编辑 fstab, 删除或注释掉不存在的设备:   ║")
		fmt.Println("  ║     vi /mnt/etc/fstab                        ║")
		fmt.Println("  ║     (注释 /data, /mnt/* 等额外磁盘条目)      ║")
		fmt.Println("  ║                                              ║")
		fmt.Println("  ║  4. 卸载并重启:                              ║")
		fmt.Println("  ║     umount /mnt                              ║")
		fmt.Println("  ║     reboot                                   ║")
		fmt.Println("  ║                                              ║")
		fmt.Println("  ║  ⚠ 不执行以上操作, 系统可能无法启动!         ║")
		fmt.Println("  ╚══════════════════════════════════════════════╝")
		fmt.Println()
	}
	warn()
	fmt.Println("  ── 以上提醒将在 10 秒后重复 ──")
	time.Sleep(10 * time.Second)
	warn()
	fmt.Println("  ── 最终提醒 ──")
	time.Sleep(10 * time.Second)
	warn()
}

// makeFileName generates a default filename like "192.168.1.100-sda.img.gz"
func makeFileName(ip, diskName string) string {
	// Clean disk name: "/dev/sda" → "sda", "sda" → "sda"
	diskName = filepath.Base(diskName)
	diskName = strings.TrimPrefix(diskName, "dev")
	return fmt.Sprintf("%s-%s.img.gz", ip, diskName)
}

func makeProgressFn() func(clone.Progress) {
	return func(p clone.Progress) {
		if p.Done {
			if p.Error == nil {
				cli.PrintProgressComplete(cli.CloneProgress{
					BytesWritten:   p.BytesWritten,
					TotalBytes:     p.TotalBytes,
					Percent:        p.Percent,
					SpeedMBps:      p.SpeedMBps,
					ElapsedSeconds: p.ElapsedSeconds,
					EtaSeconds:     p.EtaSeconds,
				})
			}
			return
		}
		cli.PrintProgress(cli.CloneProgress{
			BytesWritten:   p.BytesWritten,
			TotalBytes:     p.TotalBytes,
			Percent:        p.Percent,
			SpeedMBps:      p.SpeedMBps,
			ElapsedSeconds: p.ElapsedSeconds,
			EtaSeconds:     p.EtaSeconds,
		})
	}
}

func filterDisks(disks []disk.DiskInfo) []cli.DiskItem {
	var list []cli.DiskItem
	for _, d := range disks {
		if d.Type == "disk" {
			list = append(list, cli.DiskItem{
				Path:      d.Path,
				SizeHuman: d.SizeHuman,
				SizeBytes: d.SizeBytes,
				Model:     d.Model,
				Name:      d.Name,
			})
		}
	}
	return list
}

func countType(disks []disk.DiskInfo, t string) int {
	n := 0
	for _, d := range disks {
		if d.Type == t {
			n++
		}
	}
	return n
}
