package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"disk-cloner/internal/cli"
	"disk-cloner/internal/clone"
	"disk-cloner/internal/disk"
	"disk-cloner/internal/fixboot"
	"disk-cloner/internal/local"
	sshclient "disk-cloner/internal/ssh"
	"disk-cloner/internal/storage"
)

const (
	remoteLsblkCmd = "lsblk -Jb -o NAME,SIZE,TYPE,MOUNTPOINT,MODEL,SERIAL,TRAN,ROTA,RM,FSTYPE,LABEL"
	// Newer util-linux prefers the MOUNTPOINTS array; the legacy singular
	// mountpoint can be null when a device has several mounts. The plural
	// column is tried first and the classic set is the fallback for older
	// lsblk builds that reject the unknown column.
	remoteLsblkCmdNew = "lsblk -Jb -o NAME,SIZE,TYPE,MOUNTPOINTS,MOUNTPOINT,MODEL,SERIAL,TRAN,ROTA,RM,FSTYPE,LABEL"
	clearLine         = "\r                                                                                \r"
	version           = "3.2.0"
)

// scanRemoteDisks lists disks on a Runner (remote SSH or local), preferring
// the modern lsblk column set with MOUNTPOINTS and falling back to the
// classic one.
func scanRemoteDisks(r sshclient.Runner) ([]disk.DiskInfo, error) {
	var lastErr error
	for _, cmd := range []string{remoteLsblkCmdNew, remoteLsblkCmd} {
		out, err := r.CombinedOutput(cmd)
		if err != nil || strings.TrimSpace(out) == "" {
			if err != nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("lsblk 输出为空")
			}
			continue
		}
		disks, perr := disk.ParseJSON(out)
		if perr == nil {
			return disks, nil
		}
		lastErr = perr
	}
	return nil, lastErr
}

var compressLevel = 1 // default gzip compression level 1-9, 0 = no compression
var compressType = 0  // 0=gzip, 1=pigz (multi-threaded)
var fixInitramfs = false
var tlsVerifyEnabled = false // -tls-verify: enable certificate verification for WebDAV/S3 https

func main() {
	var (
		remoteIP    = flag.String("H", "", "远程服务器 IP")
		remotePort  = flag.Int("P", 22, "SSH 端口")
		remoteUser  = flag.String("u", "root", "SSH 用户名")
		remotePass  = flag.String("p", "", "SSH 密码 (也可用环境变量 DISK_CLONER_PASSWORD)")
		source      = flag.String("s", "", "源磁盘 (远程)")
		target      = flag.String("t", "", "目标磁盘 (本地)")
		bs          = flag.String("bs", "4M", "dd 块大小")
		compressLv  = flag.Int("z", 1, "压缩级别 0-9 (0=不压缩, 1=最快, 9=最小)")
		autoYes     = flag.Bool("y", false, "跳过确认")
		saveFile    = flag.String("o", "", "保存为 gzip 文件")
		noFixBoot   = flag.Bool("no-fix-boot", false, "跳过引导修复 (克隆和恢复模式均生效)")
		fixBootDev  = flag.String("fix-boot-disk", "", "独立修复引导")
		restoreFile = flag.String("r", "", "恢复 gzip 文件到远程磁盘")
		dst         = flag.String("dst", "", "传输到远程存储 (sftp:// ftp:// dav:// davs:// s3:// pixeldrain://)")
		localDisk   = flag.String("l", "", "本机磁盘作为源 (程序直接运行在源机 RAM OS, 需 Linux; 搭配 -dst 或 -o)")
		tlsVerify   = flag.Bool("tls-verify", false, "对 WebDAV/S3 的 HTTPS 启用证书校验 (默认关闭; URL 中也可加 tlsverify=1)")
		showVer     = flag.Bool("V", false, "显示版本号")
	)
	flag.Usage = func() {
		fmt.Println("Disk Cloner v" + version)
		fmt.Println("通过 SSH 远程克隆磁盘的 Go 工具")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  disk-cloner [参数]")
		fmt.Println()
		fmt.Println("交互模式 (直接运行):")
		fmt.Println("  disk-cloner")
		fmt.Println()
		fmt.Println("命令行模式:")
		fmt.Println("  disk-cloner -H 服务器IP -p 密码 -s /dev/sda -t /dev/sda -y")
		fmt.Println("  disk-cloner -H 服务器IP -p 密码 -s /dev/sda -o auto -y")
		fmt.Println("  disk-cloner -H 服务器IP -p 密码 -s /dev/sda -r backup.img.gz -y")
		fmt.Println("  disk-cloner -H 服务器IP -p 密码 -s /dev/sda -dst 'sftp://user:pass@nas.lan/backup/x.img.gz' -y")
		fmt.Println("  disk-cloner -l /dev/sda -dst 'sftp://user:pass@nas.lan/backup/' -y   (本机模式, 在源机 RAM OS 上运行)")
		fmt.Println()
		fmt.Println("参数:")
		flag.PrintDefaults()
		fmt.Println()
		fmt.Println("示例:")
		fmt.Println("  克隆:  disk-cloner -H 192.168.1.100 -p mypass -s /dev/sda -t /dev/sda -y")
		fmt.Println("  保存:  disk-cloner -H 192.168.1.100 -p mypass -s /dev/sda -o auto -y")
		fmt.Println("  恢复:  disk-cloner -H 192.168.1.100 -p mypass -s /dev/sda -r backup.img.gz -y")
	}
	flag.Parse()

	// Allow the SSH password via environment variable so it doesn't show up
	// in `ps` output or shell history.
	if *remotePass == "" {
		if envPass := os.Getenv("DISK_CLONER_PASSWORD"); envPass != "" {
			*remotePass = envPass
		}
	}

	compressLevel = *compressLv
	if compressLevel < 0 || compressLevel > 9 {
		compressLevel = 1
	}
	tlsVerifyEnabled = *tlsVerify

	if *showVer {
		fmt.Println("Disk Cloner v" + version)
		return
	}

	// -t (clone to disk), -o (save to file), -r (restore) and -dst (push to
	// remote storage) are mutually exclusive; accepting several would
	// silently run only one.
	ops := 0
	for _, v := range []*string{target, saveFile, restoreFile, dst} {
		if *v != "" {
			ops++
		}
	}
	if ops > 1 {
		fmt.Fprintln(os.Stderr, "错误: 参数 -t / -o / -r / -dst 只能同时指定一个")
		os.Exit(1)
	}

	// --fix-boot-disk is a standalone mode; combining it with operation
	// flags would silently ignore those flags.
	if *fixBootDev != "" {
		if *remoteIP != "" || *source != "" || *target != "" || *saveFile != "" || *restoreFile != "" || *dst != "" || *localDisk != "" {
			fmt.Fprintln(os.Stderr, "错误: -fix-boot-disk 是独立模式, 不能与其他操作参数同时使用")
			os.Exit(1)
		}
	}

	// -l has its own constraints.
	if *localDisk != "" {
		if runtime.GOOS != "linux" {
			fmt.Fprintln(os.Stderr, "错误: -l (本机模式) 仅支持在 Linux (Alpine RAM OS) 上运行")
			os.Exit(1)
		}
		if *remoteIP != "" || *source != "" || *target != "" || *restoreFile != "" {
			fmt.Fprintln(os.Stderr, "错误: -l 不能与 -H/-s/-t/-r 同时使用")
			os.Exit(1)
		}
		if (*dst == "") == (*saveFile == "") {
			fmt.Fprintln(os.Stderr, "错误: -l 需要且只能搭配 -dst 或 -o 之一")
			os.Exit(1)
		}
	}

	// Fail fast on incomplete/mismatched flag combinations instead of
	// silently dropping into interactive mode (or failing after a remote scan).
	if (*remoteIP != "") != (*source != "") {
		fmt.Fprintln(os.Stderr, "错误: -H 与 -s 必须同时指定")
		os.Exit(1)
	}
	if *remoteIP != "" && *target == "" && *saveFile == "" && *restoreFile == "" && *dst == "" {
		fmt.Fprintln(os.Stderr, "错误: 指定了 -H/-s 但未指定操作 (-t / -o / -r / -dst), 运行时不带参数可进入交互模式")
		os.Exit(1)
	}
	if *target != "" && runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "错误: Windows 不支持克隆到本地磁盘 (-t), 请使用 -o 保存为文件")
		os.Exit(1)
	}

	cli.SetupConsole()
	ensureDeps()

	if *fixBootDev != "" {
		if runtime.GOOS != "linux" {
			fmt.Println("  [!] --fix-boot-disk 需要挂载/chroot 本地磁盘, 仅支持在 Linux (Alpine RAM OS) 上运行")
			os.Exit(1)
		}
		fmt.Println()
		fmt.Println("  修复引导 - 独立模式")
		if err := fixboot.Run(fixboot.Config{TargetDisk: *fixBootDev}); err != nil {
			fmt.Printf("\n  修复失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *localDisk != "" {
		runDirectLocal(*localDisk, *bs, *autoYes, *saveFile, *dst)
		return
	}

	if *remoteIP != "" && *source != "" && (*target != "" || *saveFile != "" || *restoreFile != "" || *dst != "") {
		runDirect(*remoteIP, *remotePort, *remoteUser, *remotePass,
			*source, *target, *bs, *autoYes, *saveFile, *noFixBoot, *restoreFile, *dst)
		return
	}

	runInteractive()
}

func ensureDeps() {
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := exec.LookPath("apk"); err != nil {
		return
	}
	// Only tools the program actually runs locally: lsblk (disk scan),
	// lvm/blkid/mount (fixboot), efibootmgr (UEFI boot entry). Filesystem
	// repair/mkfs tools are only ever needed on the remote side.
	deps := []struct{ pkg, binary string }{
		{"util-linux", "lsblk"},
		{"lvm2", "lvm"},
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
		fmt.Printf("  部分依赖安装失败 (继续): %v\n", err)
	} else {
		fmt.Println("  依赖安装完成")
	}
	fmt.Println()
}

func ensureRemoteDeps(sshClient *sshclient.Client) {
	if _, err := sshClient.CombinedOutput("command -v apk"); err != nil {
		return
	}
	checks := []struct{ cmd, pkg string }{
		{"lsblk", "util-linux"},
		{"gzip", "gzip"},
		{"fsck.ext4", "e2fsprogs"},
	}
	// Note: pigz is intentionally NOT pre-installed here — buildCompressCmd
	// installs it on demand only when the user actually selects pigz.
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
	out, err := sshClient.CombinedOutput("apk add --quiet " + strings.Join(missing, " "))
	if err != nil {
		fmt.Printf("  远程依赖安装失败: %v %s\n", err, out)
	} else {
		fmt.Println("  远程依赖安装完成")
	}
}

func formatTotalTime(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%.1f秒", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%d分%d秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d时%d分%d秒", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

// authMethod returns a human-readable description of the SSH auth method
// for logging purposes. Never logs the password itself.
func authMethod(pass string) string {
	if pass == "" {
		return "密钥"
	}
	return "密码"
}

// readBlockSize prompts for the dd block size and re-prompts until the value
// passes validation — a bad block size must fail before zero-fill and
// initramfs rebuild, not after hours of pre-transfer work.
func readBlockSize() string {
	for {
		bs := cli.ReadInput("块大小", "4M")
		if err := clone.ValidateBlockSize(bs); err != nil {
			fmt.Printf("  [!] %v (示例: 4M, 1M, 512K)\n", err)
			continue
		}
		return bs
	}
}

// imageSize returns the best-known uncompressed size of a saved image:
// the .size sidecar written at save time (exact for any size), then the
// gzip ISIZE footer (exact only up to 4 GiB — ISIZE wraps modulo 2^32
// above that), then the raw file size for uncompressed .img files.
// Returns 0 when the size is unknown.
func imageSize(fileName string) int64 {
	if s := clone.ReadSizeFile(fileName); s > 0 {
		return s
	}
	if s := clone.GzipUncompressedSize(fileName); s > 0 {
		return s
	}
	if !clone.IsGzipFile(fileName) {
		if fi, err := os.Stat(fileName); err == nil {
			return fi.Size()
		}
	}
	return 0
}

// compressTypeName maps the compressType code to a human-readable name.
func compressTypeName(t int) string {
	if t == 1 {
		return "pigz"
	}
	return "gzip"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// isBack returns true when the user wants to go back to the previous menu.
func isBack(v int) bool { return v == -2 }

func runInteractive() {
	cli.PrintHeader()

	// 启动即选操作模式。模式 4 = 本机硬盘直传远程存储（程序运行在源机
	// RAM OS 上，无需 SSH）；其余模式随后连接远程服务器，后续流程与
	// 原来一致。
	var startMode int
	for {
		fmt.Println()
		fmt.Println("  操作模式 — 输入序号选择:")
		fmt.Println("  [1] 克隆到本地磁盘 (dd -> 磁盘)")
		fmt.Println("  [2] 保存为压缩文件 (dd -> gzip 文件)")
		fmt.Println("  [3] 恢复文件到远程磁盘 (gzip 文件 -> dd 远程磁盘)")
		startMinMode, maxMode := 1, 4
		if runtime.GOOS == "windows" {
			fmt.Println("  (Windows 不支持克隆到磁盘和本机传输, 其他模式可用)")
			startMinMode = 2
			maxMode = 3
		} else {
			fmt.Println("  [4] 读取本地硬盘传输到远程存储 (dd -> SFTP/FTP/WebDAV/S3)")
		}
		m := cli.SelectOption("请输入序号", startMinMode, maxMode)
		if isBack(m) {
			waitExit()
			return
		}
		if m == 4 {
			runLocalInteractive()
			continue // 返回操作模式菜单
		}
		startMode = m
		break
	}

	var sshClient *sshclient.Client
	defer func() {
		if sshClient != nil {
			sshClient.Close()
		}
	}()

	firstOp := true

connectLoop:
	for {
		// Close the previous SSH connection when reconnecting (q from mode
		// selection, or a failed attempt) so connections don't leak.
		if sshClient != nil {
			sshClient.Close()
			sshClient = nil
		}

		fmt.Println("  远程服务器配置")
		fmt.Println("  ─────────────────────────────────────────────")
		ip := cli.ReadInput("服务器IP", "")
		ip = extractIP(ip)
		if ip == "" {
			fmt.Println("  取消")
			waitExit()
			return
		}
		port := cli.ReadInt("SSH 端口", 22)
		user := cli.ReadInput("用户名", "root")
		pass := cli.ReadPassword("密码 (回车使用密钥)")
		if pass == "" {
			fmt.Println("  将尝试 SSH 密钥认证...")
		}

		fmt.Println()

		fmt.Print("  正在连接...")
		var err error
		sshClient, err = sshclient.Connect(sshclient.Config{
			Host: ip, Port: port, User: user, Password: pass, Timeout: 15,
		})
		if err != nil {
			fmt.Printf(clearLine+"  连接失败: %v\n", err)
			fmt.Println()
			fmt.Println("  常见原因:")
			fmt.Println("    - 密码错误")
			fmt.Println("    - 服务器只允许密钥认证")
			fmt.Println("    - 防火墙拦截了连接")
			fmt.Println()
			fmt.Println("  请重新输入连接信息...")
			fmt.Println()
			continue
		}
		fmt.Printf(clearLine+"  SSH 连接成功 (%s@%s:%d)\n", user, ip, port)
		if fp := sshClient.ServerKeyFingerprint; fp != "" {
			fmt.Printf("  主机密钥: %s (未做已知主机校验, 请在可信网络使用)\n", fp)
		}

		// Start capturing the session for the optional Mode 2 log file.
		// Entries are buffered until a save path is known; if the user picks
		// another mode or cancels, the buffer is discarded silently.
		logger := newSessionLogger()
		logger.logf("=== Disk Cloner v%s 会话开始 ===", version)
		logger.logf("SSH 连接: %s@%s:%d (认证: %s)", user, ip, port, authMethod(pass))

		fmt.Println()
		fmt.Println("  ─────────────────────────────────────────────")
		readiness := checkRemoteReadiness(sshClient, "远程")
		logger.logf("远程环境: OS=%q RootFS=%q Alpine=%v RAM=%v Detected=%v",
			readiness.OSLine, readiness.RootFS, readiness.IsAlpine, readiness.IsRAM, readiness.Detected)
		if readiness.Detected && !readiness.IsSafe() {
			logger.logf("警告: 远程不是 Alpine RAM OS,继续可能导致数据不一致")
		}
		if !confirmUnsafeRemote(readiness, false, "远程") {
			logger.logf("用户取消: 远程不是 Alpine RAM OS")
			fmt.Println("  已取消,请先将远程重启进入 Alpine RAM OS 后再试")
			fmt.Println()
			continue
		}
		ensureRemoteDeps(sshClient)
		logger.logf("远程依赖检查完成")

		fmt.Println()
		fmt.Println("  ─────────────────────────────────────────────")
		fmt.Print("  正在扫描远程磁盘...")
		remoteDisks, err := scanRemoteDisks(sshClient)
		if err != nil {
			msg := err.Error()
			logger.logf("远程磁盘扫描失败: %s", msg)
			fmt.Printf(clearLine+"  远程扫描失败: %s\n", msg)
			fmt.Println("    请确认远程已安装 lsblk (apk add util-linux)")
			fmt.Println()
			continue
		}
		fmt.Printf(clearLine+"  发现 %d 块远程磁盘\n", countType(remoteDisks, "disk"))
		logger.logf("扫描到 %d 块远程磁盘", countType(remoteDisks, "disk"))
		remoteList := filterDisks(remoteDisks)
		for _, d := range remoteList {
			model := d.Model
			if model == "" {
				model = "(无型号)"
			}
			logger.logf("  远程磁盘: %s  大小 %s  %s", d.Path, d.SizeHuman, model)
		}
		if len(remoteList) == 0 {
			fmt.Println("\n  远程未发现磁盘设备")
			continue
		}

		fmt.Println()
		cli.PrintSection(fmt.Sprintf("远程磁盘 (%s)", ip))
		cli.PrintDiskList(remoteList, "remote")

		// Local disks are only needed for clone mode; Windows supports
		// save/restore only.
		isWindows := runtime.GOOS == "windows"
		var localList []cli.DiskItem
		if !isWindows {
			fmt.Println()
			fmt.Println("  ─────────────────────────────────────────────")
			fmt.Print("  正在扫描本地磁盘...")
			localDisks, err := disk.GetLocalDisks()
			if err != nil {
				fmt.Printf(clearLine+"  本地扫描失败: %v\n", err)
				fmt.Println("    请确认本地已安装 lsblk (apk add util-linux)")
				fmt.Println()
				fmt.Println("  可使用保存为文件模式继续")
				fmt.Println()
			}
			localList = filterDisks(localDisks)

			if len(localList) > 0 {
				cli.PrintSection("本地磁盘")
				cli.PrintDiskList(localList, "local")
			}
		}

		// Operation loop: stays on the same SSH connection so the user can
		// run several operations in a row ("继续其他操作? yes").
	opLoop:
		for {
			// Refresh the local disk list between operations (a clone may
			// have changed the disks attached locally).
			if !isWindows {
				if localDisks, err := disk.GetLocalDisks(); err == nil {
					localList = filterDisks(localDisks)
				}
			}

			// The operation mode was chosen at startup; on later rounds of
			// "继续其他操作" the menu re-appears so several different
			// operations can run in a row on the same connection.
			mode := startMode
			if !firstOp {
				fmt.Println()
				fmt.Println("  操作模式 — 输入序号选择:")
				fmt.Println("  [1] 克隆到本地磁盘 (dd -> 磁盘)")
				fmt.Println("  [2] 保存为压缩文件 (dd -> gzip 文件)")
				fmt.Println("  [3] 恢复文件到远程磁盘 (gzip 文件 -> dd 远程磁盘)")
				minMode, maxMode := 1, 3
				if isWindows {
					minMode = 2
				}
				mode = cli.SelectOption("请输入序号", minMode, maxMode)
				if isBack(mode) {
					// q → back to SSH configuration (re-connect menu)
					continue connectLoop
				}
			}
			firstOp = false
			logger.logf("用户选择操作模式: %d", mode)

			// Disk selection: q goes back one step — target disk select →
			// source disk select → operation mode.
			var srcDisk, tgtDisk cli.DiskItem
			batchAll := false
		selLoop:
			for {
				cli.PrintSection("选择源磁盘 — 输入序号选定远程磁盘")
				cli.PrintDiskList(remoteList, "remote")
				if mode == 2 {
					fmt.Println("  [0] 备份全部磁盘")
				}
				minIdx := 1
				if mode == 2 {
					minIdx = 0
				}
				srcIdx := cli.SelectDisk("请输入序号", minIdx, len(remoteList))
				if isBack(srcIdx) {
					continue opLoop
				}
				if mode == 2 && srcIdx == 0 {
					batchAll = true
					logger.logf("用户选择备份全部磁盘")
				} else {
					srcDisk = remoteList[srcIdx-1]
					logger.logf("用户选择源磁盘: %s (%s)", srcDisk.Path, srcDisk.SizeHuman)
				}

				if mode == 1 {
					if len(localList) == 0 {
						fmt.Println("\n  本地未发现磁盘, 无法克隆")
						fmt.Println("  请重新输入或选择保存为文件模式")
						fmt.Println()
						continue opLoop
					}
					cli.PrintSection("选择目标磁盘 — 输入序号选定本地磁盘")
					cli.PrintDiskList(localList, "local")
					tgtIdx := cli.SelectDisk("请输入序号", 1, len(localList))
					if isBack(tgtIdx) {
						continue selLoop
					}
					tgtDisk = localList[tgtIdx-1]
				}
				break
			}

			if batchAll {
				batchSaveToFile(ip, remoteList, sshClient, logger)
			} else if mode == 1 {
				fmt.Println()
				fmt.Println("  +--------------------------------------------+")
				fmt.Printf("  |  源:   %s:%s (%s)\n", ip, srcDisk.Path, srcDisk.SizeHuman)
				fmt.Printf("  |  目标: 本地 %s (%s)\n", tgtDisk.Path, tgtDisk.SizeHuman)
				fmt.Println("  +--------------------------------------------+")

				if tgtDisk.SizeBytes < srcDisk.SizeBytes {
					fmt.Printf("\n  警告: 目标盘 (%s) 小于源盘 (%s),整盘克隆会被截断,恢复后目标文件系统将损坏!\n",
						tgtDisk.SizeHuman, srcDisk.SizeHuman)
					if !cli.Confirm("  确认仍要强制克隆? 输入 yes 继续") {
						fmt.Println("  已取消")
						continue
					}
				}

				blockSize := readBlockSize()
				compressLevel = cli.AskCompressionLevel()
				compressType = cli.AskCompressionType()

				fmt.Println()
				doZero := cli.ConfirmZero()
				fixInitramfs = cli.AskFixInitramfs()
				fmt.Println()

				fmt.Printf("  此操作将覆盖 %s 上的所有数据!\n", tgtDisk.Path)
				if !cli.Confirm("  确认开始克隆? 输入 yes 继续") {
					fmt.Println("  已取消")
					fmt.Println()
					continue
				}

				fmt.Println()
				fmt.Println("  开始克隆...")
				fmt.Println()

				totalStart := time.Now()
				job := clone.New(sshClient, clone.Params{
					SourcePath:       srcDisk.Path,
					TargetPath:       tgtDisk.Path,
					SourceSize:       srcDisk.SizeBytes,
					BlockSize:        blockSize,
					ZeroFill:         doZero,
					CompressionLevel: compressLevel,
					CompressType:     compressType,
					FixInitramfs:     fixInitramfs,
				}, makeProgressFn())
				job.SetLogFunc(func(format string, args ...interface{}) {
					fmt.Printf(format+"\n", args...)
				})

				if err := job.Run(); err != nil {
					fmt.Printf("\n  克隆失败: %v\n", err)
					fmt.Println()
					continue
				}

				fmt.Println("  克隆完成!")
				fmt.Println()

				// Reinstall GRUB + rebuild initramfs on the freshly cloned
				// disk. Without this, the cloned disk will fail to boot with
				// "GRUB: unknown filesystem" because the source's core.img
				// embeds block addresses that don't match the new disk
				// geometry, and the source's initramfs only has drivers for
				// the source hardware.
				fmt.Println("  修复引导（确保目标盘可启动）...")
				if err := fixboot.Run(fixboot.Config{TargetDisk: tgtDisk.Path}); err != nil {
					fmt.Printf("  [!] 引导修复失败: %v（克隆已成功，请手动修复引导）\n", err)
				}

				fmt.Println("  ===============================================")
				fmt.Printf("  全部完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))
				fmt.Println("  ===============================================")
			} else if mode == 2 {
				runSaveToFile(ip, srcDisk, sshClient, logger)
			} else {
				runRestoreToRemote(ip, srcDisk, sshClient)
			}

			if !cli.Confirm("  继续其他操作? 输入 yes 继续，其他退出") {
				waitExit()
				return
			}
			// yes → back to the operation mode prompt, SSH connection kept.
		}
	}
}

func runDirect(ip string, port int, user, pass, source, target, bs string,
	autoYes bool, saveFile string, noFixBoot bool, restoreFile string, dst string) {

	if err := clone.ValidateDevicePath(source); err != nil {
		log.Fatalf("无效的磁盘路径 %q: %v", source, err)
	}
	if err := clone.ValidateBlockSize(bs); err != nil {
		log.Fatalf("无效的块大小 %q: %v (示例: 4M, 1M, 512K)", bs, err)
	}
	// Validate the storage URL before dialing: a malformed -dst should not
	// wait for an SSH connection to be rejected.
	var dstCfg *storage.Config
	if dst != "" {
		cfg, err := storage.ParseURL(dst)
		if err != nil {
			log.Fatalf("%v", err)
		}
		if tlsVerifyEnabled {
			cfg.InsecureTLS = false
		}
		dstCfg = &cfg
	}

	sshClient, err := sshclient.Connect(sshclient.Config{
		Host: ip, Port: port, User: user, Password: pass, Timeout: 30,
	})
	if err != nil {
		log.Fatalf("SSH 连接失败: %v", err)
	}
	defer sshClient.Close()
	if fp := sshClient.ServerKeyFingerprint; fp != "" {
		fmt.Printf("主机密钥: %s (未做已知主机校验)\n", fp)
	}

	ensureRemoteDeps(sshClient)

	// Detect Alpine RAM OS — refuse or warn if remote is running a normal system
	readiness := checkRemoteReadiness(sshClient, "远程")
	if !confirmUnsafeRemote(readiness, autoYes, "远程") {
		fmt.Println("已取消")
		return
	}

	// Direct mode defaults: rebuild initramfs for cross-hardware boot
	// compatibility (applies to both save and clone paths; restore ignores it).
	fixInitramfs = true

	remoteDisks, err := scanRemoteDisks(sshClient)
	if err != nil {
		log.Fatalf("远程扫描失败: %v\n  请确认远程已安装 lsblk (apk add util-linux)", err)
	}
	srcDisk := disk.FindDisk(remoteDisks, source)
	if srcDisk == nil {
		log.Fatalf("远程磁盘未找到: %s", source)
	}
	fmt.Printf("远程: %s:%s (%s)\n", ip, source, srcDisk.SizeHuman)

	cliDisk := cli.DiskItem{Path: srcDisk.Path, SizeBytes: srcDisk.SizeBytes, SizeHuman: srcDisk.SizeHuman, Name: srcDisk.Name}

	if saveFile != "" {
		if saveFile == "auto" {
			dateStr := time.Now().Format("2006-01-02")
			dateDir := filepath.Join(".", dateStr)
			if err := os.MkdirAll(dateDir, 0755); err != nil {
				log.Fatalf("无法创建保存目录 %s: %v", dateDir, err)
			}
			saveFile = filepath.Join(dateDir, makeFileName(ip, source, srcDisk.SizeHuman, dateStr, saveExtFor(compressLevel)))
		}
		// Level 0 writes raw bytes — don't leave a user-supplied .img.gz
		// name implying gzip content (the interactive path applies the
		// same rule in doSaveToFile).
		if compressLevel == 0 && strings.HasSuffix(saveFile, ".img.gz") {
			saveFile = strings.TrimSuffix(saveFile, ".img.gz") + ".img"
		}
		if compressLevel == 0 {
			fmt.Printf("文件: %s (不压缩)\n", saveFile)
		} else {
			fmt.Printf("文件: %s (gzip)\n", saveFile)
		}

		// Direct (command-line) save: use flag values directly instead of
		// prompting for block size / compression level / compression type.
		logger := newSessionLogger()
		logger.logf("=== Disk Cloner v%s 命令行模式 ===", version)
		logger.logf("SSH 连接: %s@%s:%d (认证: %s)", user, ip, port, authMethod(pass))
		logger.logf("远程环境: OS=%q RootFS=%q Alpine=%v RAM=%v Detected=%v",
			readiness.OSLine, readiness.RootFS, readiness.IsAlpine, readiness.IsRAM, readiness.Detected)
		logger.logf("源磁盘: %s (%s)", srcDisk.Path, srcDisk.SizeHuman)
		logger.logf("保存文件: %s", saveFile)
		logger.logf("块大小: %s  压缩级别: %d  压缩方式: %s  零填充: 是  重建 initramfs: %v",
			bs, compressLevel, compressTypeName(compressType), fixInitramfs)

		if !autoYes {
			fmt.Printf("将保存远程 %s 到文件 %s\n", srcDisk.Path, saveFile)
			if !cli.Confirm("确认继续? 输入 yes") {
				fmt.Println("已取消")
				return
			}
		}

		logPath := saveFile + ".log"
		if err := logger.open(logPath); err != nil {
			fmt.Printf("  [!] 无法创建日志文件 %s: %v (继续无日志)\n", logPath, err)
		} else {
			fmt.Printf("  日志文件: %s\n", logPath)
		}
		execSaveToFile(ip, cliDisk, sshClient, saveFile, bs, true, logger)
		logger.close()
		return
	}

	if dst != "" {
		cfg := *dstCfg
		dateStr := time.Now().Format("2006-01-02")
		if cfg.NeedsName() {
			cfg.AppendName(makeFileName(ip, source, srcDisk.SizeHuman, dateStr, saveExtFor(compressLevel)))
		}
		fmt.Printf("目标: %s\n", storage.Describe(cfg))

		logger := newSessionLogger()
		logger.logf("=== Disk Cloner v%s 命令行模式 (远程存储) ===", version)
		logger.logf("SSH 连接: %s@%s:%d (认证: %s)", user, ip, port, authMethod(pass))
		logger.logf("远程环境: OS=%q RootFS=%q Alpine=%v RAM=%v Detected=%v",
			readiness.OSLine, readiness.RootFS, readiness.IsAlpine, readiness.IsRAM, readiness.Detected)
		logger.logf("源磁盘: %s (%s)", srcDisk.Path, srcDisk.SizeHuman)
		logger.logf("存储目标: %s", storage.Describe(cfg))
		logger.logf("块大小: %s  压缩级别: %d  压缩方式: %s  零填充: 是  重建 initramfs: %v",
			bs, compressLevel, compressTypeName(compressType), fixInitramfs)

		if !autoYes {
			fmt.Printf("将读取远程 %s 并流式上传到 %s\n", srcDisk.Path, storage.Describe(cfg))
			if !cli.Confirm("确认继续? 输入 yes") {
				fmt.Println("已取消")
				return
			}
		}

		dateDir := filepath.Join(".", dateStr)
		if err := os.MkdirAll(dateDir, 0755); err != nil {
			log.Fatalf("无法创建日志目录 %s: %v", dateDir, err)
		}
		logPath := filepath.Join(dateDir, storage.BaseName(cfg)+".log")
		if err := logger.open(logPath); err != nil {
			fmt.Printf("  [!] 无法创建日志文件 %s: %v (继续无日志)\n", logPath, err)
		} else {
			fmt.Printf("  日志文件: %s\n", logPath)
		}
		execSaveToStorage(cliDisk, sshClient, cfg, bs, true, dateDir, logger)
		logger.close()
		return
	}

	if restoreFile != "" {
		if _, err := os.Stat(restoreFile); err != nil {
			log.Fatalf("文件不存在: %s", restoreFile)
		}
		fmt.Printf("文件: %s -> 远程: %s (%s)\n", restoreFile, source, srcDisk.SizeHuman)

		// Verify file integrity if a .sha256 checksum file exists
		if !verifyChecksum(restoreFile) {
			if !autoYes {
				if !cli.Confirm("  校验失败，是否继续恢复? 输入 yes 强制恢复") {
					fmt.Println("已取消")
					return
				}
			} else {
				fmt.Println("  [!] 校验失败，-y 模式继续恢复（风险自负）")
			}
		}

		// Check target disk size vs uncompressed image size.
		uncompSize := imageSize(restoreFile)
		if uncompSize > 0 {
			if targetSize, _ := getRemoteDiskSize(sshClient, source); targetSize > 0 && uncompSize > targetSize {
				pct := float64(targetSize) / float64(uncompSize) * 100
				fmt.Printf("  [!] 目标盘 (%s) 小于解压后镜像 (%s)，只能写入约 %.1f%%\n",
					disk.FormatBytes(targetSize), disk.FormatBytes(uncompSize), pct)
				if !autoYes {
					if !cli.Confirm("  继续恢复? 输入 yes") {
						fmt.Println("已取消")
						return
					}
				} else {
					fmt.Println("  [!] -y 模式继续恢复（风险自负）")
				}
			}
		}

		totalStart := time.Now()
		// Restore consumes no compression/zero-fill/initramfs params — only
		// TargetPath/BlockSize/SourceSize matter here, so leave the rest at
		// zero values rather than passing stale globals.
		job := clone.New(sshClient, clone.Params{
			TargetPath: source,
			BlockSize:  bs,
		}, makeProgressFn())
		job.SetLogFunc(func(format string, args ...interface{}) {
			fmt.Printf(format+"\n", args...)
		})
		if err := job.RestoreFromFile(restoreFile); err != nil {
			fmt.Printf("\n恢复失败: %v\n", err)
			os.Exit(1)
		}

		// Reinstall GRUB + rebuild initramfs on the restored disk so it boots
		// on this target hardware. See FixBoot docs for why this is mandatory.
		// Honored -no-fix-boot here as well as in the clone path.
		if !noFixBoot {
			fmt.Println("  正在修复引导（GRUB + initramfs）...")
			if err := job.FixBoot(source); err != nil {
				fmt.Printf("  [!] 引导修复失败: %v（恢复已成功，请手动修复引导）\n", err)
			}
		} else {
			fmt.Println("  [!] 已按 -no-fix-boot 跳过引导修复 — 若克隆自不同硬件, 目标盘可能无法启动")
		}

		fmt.Printf("恢复完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))

		// Post-restore verification: read-only fsck on all target partitions.
		// Catches torn images before the user reboots into a corrupted system.
		fmt.Println("  正在验证目标文件系统一致性 (只读 fsck)...")
		if bad := postRestoreFsck(sshClient, source); len(bad) > 0 {
			fmt.Printf("  [!] 警告: 以下分区 fsck 报告错误: %s\n", strings.Join(bad, ", "))
			fmt.Println("  [!] 恢复的文件系统可能不一致,重启前请先执行:")
			for _, p := range bad {
				fmt.Printf("        fsck -fy %s\n", p)
			}
		} else {
			fmt.Println("  ✓ 目标文件系统一致性检查通过")
		}
		return
	}

	if runtime.GOOS == "windows" {
		log.Fatal("Windows 不支持克隆到磁盘，请使用 -o 保存为文件")
	}

	localDisks, err := disk.GetLocalDisks()
	if err != nil {
		log.Fatalf("本地扫描失败: %v", err)
	}
	tgtDisk := disk.FindDisk(localDisks, target)
	if tgtDisk == nil {
		log.Fatalf("本地磁盘未找到: %s", target)
	}
	fmt.Printf("本地: %s (%s)\n", target, tgtDisk.SizeHuman)

	if tgtDisk.SizeBytes < srcDisk.SizeBytes {
		fmt.Printf("警告: 目标盘 (%s) 小于源盘 (%s)\n",
			tgtDisk.SizeHuman, srcDisk.SizeHuman)
	}

	if !autoYes {
		fmt.Printf("此操作将覆盖 %s 上的所有数据!\n", target)
		if !cli.Confirm("确认继续? 输入 yes") {
			fmt.Println("已取消")
			return
		}
	}

	fmt.Println("\n开始克隆...")
	totalStart := time.Now()

	job := clone.New(sshClient, clone.Params{
		SourcePath:       source,
		TargetPath:       target,
		SourceSize:       srcDisk.SizeBytes,
		BlockSize:        bs,
		ZeroFill:         true,
		CompressionLevel: compressLevel,
		CompressType:     compressType,
		FixInitramfs:     fixInitramfs,
	}, makeProgressFn())
	job.SetLogFunc(func(format string, args ...interface{}) {
		fmt.Printf(format+"\n", args...)
	})

	if err := job.Run(); err != nil {
		fmt.Printf("\n克隆失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("克隆完成!")
	fmt.Printf("总耗时: %s\n", formatTotalTime(time.Since(totalStart)))

	// Reinstall GRUB + rebuild initramfs on the freshly cloned disk so it
	// boots on this target hardware (the cloned image still has the source
	// machine's GRUB and initramfs, which won't work as-is on different HW).
	if !noFixBoot {
		fmt.Println("  正在修复引导（GRUB + initramfs）...")
		if err := fixboot.Run(fixboot.Config{TargetDisk: target}); err != nil {
			fmt.Printf("  [!] 引导修复失败: %v（克隆已成功，请手动修复引导）\n", err)
			printFstabWarning(target, autoYes)
		}
	} else {
		printFstabWarning(target, autoYes)
	}
}

func runSaveToFile(ip string, srcDisk cli.DiskItem, sshClient *sshclient.Client, logger *sessionLogger) {
	saveDir := askSaveDirectory()
	dateStr := time.Now().Format("2006-01-02")
	dateDir := filepath.Join(saveDir, dateStr)
	if err := os.MkdirAll(dateDir, 0755); err != nil {
		fmt.Printf("  [!] 无法创建保存目录 %s: %v\n", dateDir, err)
		return
	}
	fmt.Printf("  保存目录: %s\n", dateDir)
	logger.logf("保存目录: %s", dateDir)

	defaultName := makeFileName(ip, srcDisk.Name, srcDisk.SizeHuman, dateStr, ".img.gz")
	fileName := filepath.Join(dateDir, defaultName)
	fileName = cli.ReadInput("文件名", fileName)
	logger.logf("用户输入文件名: %s", fileName)
	doSaveToFile(ip, srcDisk, sshClient, fileName, logger)
}

// batchSaveToFile saves all remote disks with a single set of prompts.
func batchSaveToFile(ip string, disks []cli.DiskItem, sshClient *sshclient.Client, logger *sessionLogger) {
	saveDir := askSaveDirectory()
	dateStr := time.Now().Format("2006-01-02")
	dateDir := filepath.Join(saveDir, dateStr)
	if err := os.MkdirAll(dateDir, 0755); err != nil {
		fmt.Printf("  [!] 无法创建保存目录 %s: %v\n", dateDir, err)
		return
	}
	fmt.Printf("  保存目录: %s\n", dateDir)
	logger.logf("批量备份保存目录: %s", dateDir)
	logger.logf("待备份磁盘数: %d", len(disks))

	blockSize := readBlockSize()
	compressLevel = cli.AskCompressionLevel()
	compressType = cli.AskCompressionType()
	doZero := cli.ConfirmZero()
	fixInitramfs = cli.AskFixInitramfs()
	logger.logf("块大小: %s", blockSize)
	logger.logf("压缩级别: %d  压缩方式: %s", compressLevel, compressTypeName(compressType))
	logger.logf("零填充空闲空间: %v", doZero)
	logger.logf("重建 initramfs: %v", fixInitramfs)
	fmt.Println()
	if !cli.Confirm("  确认开始批量备份? 输入 yes 继续") {
		logger.logf("用户取消批量备份")
		fmt.Println("  已取消")
		return
	}
	logger.logf("用户确认开始批量备份")

	// Close each disk's log at the start of the NEXT iteration so the
	// completion line below can be appended to the last disk's log file.
	var lastDiskLogger *sessionLogger
	for i, d := range disks {
		fmt.Println()
		fmt.Printf("  --- 备份 %d/%d: %s ---\n", i+1, len(disks), d.Path)
		fileName := filepath.Join(dateDir, makeFileName(ip, d.Name, d.SizeHuman, dateStr, saveExtFor(compressLevel)))

		// Each disk gets its own log file containing the common preamble
		// (connection / readiness / scan) inherited via fork() plus the
		// per-disk entries below.
		diskLogger := logger.fork()
		diskLogger.logf("=== 备份 %d/%d: %s (%s) ===", i+1, len(disks), d.Path, d.SizeHuman)
		diskLogger.logf("保存文件: %s", fileName)
		logPath := fileName + ".log"
		if err := diskLogger.open(logPath); err != nil {
			fmt.Printf("  [!] 无法创建日志文件 %s: %v (继续无日志)\n", logPath, err)
		} else {
			fmt.Printf("  日志文件: %s\n", logPath)
		}
		execSaveToFile(ip, d, sshClient, fileName, blockSize, doZero, diskLogger)
		if lastDiskLogger != nil {
			lastDiskLogger.close()
		}
		lastDiskLogger = diskLogger
	}
	if lastDiskLogger != nil {
		lastDiskLogger.logf("批量备份全部完成")
		lastDiskLogger.close()
	}
}

func askSaveDirectory() string {
	cwd, _ := os.Getwd()
	fmt.Println()
	fmt.Printf("  当前目录: %s\n", cwd)

	if runtime.GOOS == "windows" {
		fmt.Println("  ─────────────────────────────────────────────")
		fmt.Println("  回车 → 使用当前目录")
		fmt.Println("  输入 b → 浏览文件夹")
		fmt.Println("  输入路径 → 保存到该路径")
		input := cli.ReadInput("  保存目录", ".")
		lower := strings.ToLower(input)
		if lower == "." || lower == "" {
			return "."
		}
		if lower == "b" || lower == "browse" {
			if dir := windowsFolderDialog(); dir != "" {
				return dir
			}
			return "."
		}
		return input
	}

	dir := cli.ReadInput("  保存目录 (回车使用当前目录，或输入路径)", ".")
	if dir == "" || dir == "." {
		return "."
	}
	return dir
}

func doSaveToFile(ip string, srcDisk cli.DiskItem, sshClient *sshclient.Client, fileName string, logger *sessionLogger) {
	fmt.Println()
	fmt.Println("  +--------------------------------------------+")
	fmt.Printf("  |  源:   %s:%s (%s)\n", ip, srcDisk.Path, srcDisk.SizeHuman)
	fmt.Printf("  |  文件: %s\n", fileName)
	fmt.Println("  +--------------------------------------------+")
	logger.logf("源: %s:%s (%s)", ip, srcDisk.Path, srcDisk.SizeHuman)
	logger.logf("保存文件: %s", fileName)
	if runtime.GOOS != "windows" {
		fmt.Println()
		fmt.Println("  注意: 如果在 RAM OS 中运行,")
		fmt.Println("    文件将写入内存文件系统，请确保内存充足")
		fmt.Println("    或将文件保存到已挂载的物理磁盘路径")
	}
	blockSize := readBlockSize()
	logger.logf("块大小: %s", blockSize)

	compressLevel = cli.AskCompressionLevel()
	compressType = cli.AskCompressionType()

	// Level 0 saves an uncompressed raw image — adjust the extension so the
	// file isn't named .img.gz while containing raw bytes.
	if compressLevel == 0 && strings.HasSuffix(fileName, ".img.gz") {
		fileName = strings.TrimSuffix(fileName, ".img.gz") + ".img"
		fmt.Printf("  不压缩: 保存为原始镜像 %s\n", fileName)
		logger.logf("不压缩模式: 文件名调整为 %s", fileName)
	}
	// Print the format only now that it is actually decided (the summary box
	// above used to claim gzip before the user chose level 0).
	if compressLevel == 0 {
		fmt.Printf("  格式: 不压缩 (原始镜像)\n")
		logger.logf("格式: 不压缩 (原始镜像)")
	} else {
		fmt.Printf("  格式: %s 压缩 (级别 %d)\n", compressTypeName(compressType), compressLevel)
		logger.logf("格式: %s 压缩 (级别 %d)", compressTypeName(compressType), compressLevel)
	}

	doZero := cli.ConfirmZero()
	fixInitramfs = cli.AskFixInitramfs()
	logger.logf("零填充空闲空间: %v", doZero)
	logger.logf("重建 initramfs: %v", fixInitramfs)
	fmt.Println()
	if !cli.Confirm("  确认开始保存? 输入 yes 继续") {
		logger.logf("用户取消保存")
		fmt.Println("  已取消")
		return
	}
	logger.logf("用户确认开始保存")

	// Now that the user has committed, create the log file. Everything
	// buffered so far (connection, readiness, scan, prompts, choices) is
	// flushed in one shot; subsequent entries are written live.
	logPath := fileName + ".log"
	if err := logger.open(logPath); err != nil {
		fmt.Printf("  [!] 无法创建日志文件 %s: %v (继续无日志)\n", logPath, err)
	} else {
		fmt.Printf("  日志文件: %s\n", logPath)
	}
	defer logger.close()

	execSaveToFile(ip, srcDisk, sshClient, fileName, blockSize, doZero, logger)
}

// execSaveToFile runs the actual save without prompts.
func execSaveToFile(ip string, srcDisk cli.DiskItem, runner sshclient.Runner, fileName string, blockSize string, doZero bool, logger *sessionLogger) {
	fmt.Println()
	fmt.Println("  开始保存...")
	fmt.Println()

	totalStart := time.Now()
	logger.logf("开始保存操作 (dd -> 网络 -> 文件, 请求压缩: %s)", compressTypeName(compressType))
	logger.logf("开始时间: %s", totalStart.Format("2006-01-02 15:04:05"))

	job := clone.New(runner, clone.Params{
		SourcePath:       srcDisk.Path,
		TargetPath:       fileName,
		SourceSize:       srcDisk.SizeBytes,
		BlockSize:        blockSize,
		ZeroFill:         doZero,
		CompressionLevel: compressLevel,
		CompressType:     compressType,
		FixInitramfs:     fixInitramfs,
	}, makeProgressFnWithLogger(logger))
	job.SetLogFunc(func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		fmt.Printf(format+"\n", args...)
		logger.logf("  %s", msg)
	})
	if err := job.RunToFile(); err != nil {
		logger.logf("保存失败: %v", err)
		fmt.Printf("\n  保存失败: %v\n", err)
		// Mark the leftover partial file so it can't be mistaken for a
		// valid backup later.
		if _, statErr := os.Stat(fileName); statErr == nil {
			partial := fileName + ".partial"
			if rnErr := os.Rename(fileName, partial); rnErr == nil {
				logger.logf("未完成的文件已重命名为 %s", partial)
				fmt.Printf("  [!] 未完成的文件已重命名为: %s\n", partial)
			}
		}
		return
	}
	logger.logf("传输完成")
	logger.logf("实际使用压缩器: %s", job.CompressToolName())

	// The checksum was computed while streaming; only recompute (slow second
	// pass) if the job didn't provide one.
	if job.ChecksumHex != "" {
		writeChecksumFile(fileName, job.ChecksumHex)
	} else {
		saveChecksum(fileName)
	}
	// Record the exact uncompressed size: gzip's ISIZE footer wraps above
	// 4 GiB, and restores need the real size to warn about small targets.
	clone.WriteSizeFile(fileName, srcDisk.SizeBytes)
	logger.logf("校验/大小文件已生成: %s.sha256 / %s.size", fileName, fileName)

	if info, err := os.Stat(fileName); err == nil {
		ratio := 0.0
		if srcDisk.SizeBytes > 0 {
			ratio = float64(info.Size()) / float64(srcDisk.SizeBytes) * 100
		}
		logger.logf("文件大小: %s (压缩率 %.1f%%)", disk.FormatBytes(info.Size()), ratio)
		fmt.Printf("  文件大小: %s (压缩率 %.1f%%)\n", disk.FormatBytes(info.Size()), ratio)
	}
	totalTime := time.Since(totalStart)
	logger.logf("总耗时: %s", formatTotalTime(totalTime))
	logger.logf("=== 保存完成 ===")
	fmt.Println()
	fmt.Println("  ===============================================")
	fmt.Printf("  保存完成! 总耗时: %s\n", formatTotalTime(totalTime))
	fmt.Println("  ===============================================")
}

// ─── 模式 4: 传输到远程存储 ────────────────────────────────────────────

// runLocalInteractive is interactive mode 4: running directly on the source
// machine (Alpine RAM OS), the LOCAL disk is dd'd and streamed to a remote
// storage service (SFTP/FTP/WebDAV/S3) without any SSH connection and
// without writing the image to a local disk. Only the small
// .sha256/.size/.log sidecars stay local.
func runLocalInteractive() {
	runner := local.NewRunner()

	fmt.Println()
	fmt.Println("  模式 4 — 本机硬盘 → 远程存储")
	fmt.Println("  程序运行在源机 RAM OS 上, 直接读本机物理硬盘, 镜像不落本地磁盘")

	for {
		fmt.Println()
		fmt.Println("  ─────────────────────────────────────────────")
		fmt.Print("  正在扫描本机磁盘...")
		disks, err := disk.GetLocalDisks()
		if err != nil {
			fmt.Printf(clearLine+"  本机磁盘扫描失败: %v\n      请确认已安装 lsblk (apk add util-linux)\n", err)
			return
		}
		localList := filterDisks(disks)
		if len(localList) == 0 {
			fmt.Println(clearLine + "  未发现本机磁盘设备")
			return
		}
		fmt.Printf(clearLine+"  发现 %d 块本机磁盘\n", len(localList))

		fmt.Println()
		cli.PrintSection("本机磁盘 (dd 源)")
		cli.PrintDiskList(localList, "local")
		idx := cli.SelectDisk("请输入序号", 1, len(localList))
		if isBack(idx) {
			return // 返回操作模式菜单
		}
		srcDisk := localList[idx-1]

		// The source here IS this machine — it must be in RAM OS too.
		readiness := checkRemoteReadiness(runner, "本机")
		logger := newSessionLogger()
		logger.logf("=== Disk Cloner v%s 本机模式 ===", version)
		logger.logf("源磁盘: %s (%s)", srcDisk.Path, srcDisk.SizeHuman)
		logger.logf("本机环境: OS=%q RootFS=%q Alpine=%v RAM=%v Detected=%v",
			readiness.OSLine, readiness.RootFS, readiness.IsAlpine, readiness.IsRAM, readiness.Detected)
		if !confirmUnsafeRemote(readiness, false, "本机") {
			logger.logf("用户取消: 本机不是 Alpine RAM OS")
			fmt.Println("  已取消,请先将本机重启进入 Alpine RAM OS 后再试")
			continue
		}

		cfg, ok := askStorageConn()
		if !ok {
			fmt.Println("  已取消")
			continue
		}
		logger.logf("存储类型: %s", cfg.Kind)

		blockSize, doZero := askTransferSettings(logger)

		dateStr := time.Now().Format("2006-01-02")
		host, _ := os.Hostname()
		if host == "" {
			host = "local"
		}
		defaultName := makeFileName(host, srcDisk.Name, srcDisk.SizeHuman, dateStr, saveExtFor(compressLevel))
		if !fillDestPath(&cfg, defaultName) {
			fmt.Println("  已取消")
			continue
		}
		logger.logf("存储目标: %s", storage.Describe(cfg))

		saveDir := askSaveDirectory()
		dateDir := filepath.Join(saveDir, dateStr)
		if err := os.MkdirAll(dateDir, 0755); err != nil {
			fmt.Printf("  [!] 无法创建校验文件目录 %s: %v\n", dateDir, err)
			continue
		}
		base := storage.BaseName(cfg)
		logger.logf("本地校验文件目录: %s", dateDir)

		fmt.Println()
		fmt.Println("  +--------------------------------------------+")
		fmt.Printf("  |  源:   本机 %s (%s)\n", srcDisk.Path, srcDisk.SizeHuman)
		fmt.Printf("  |  目标: %s\n", storage.Describe(cfg))
		fmt.Println("  |  镜像不落本地磁盘, 直接流式上传")
		fmt.Printf("  |  本地校验文件: %s.sha256/.size\n", base)
		fmt.Println("  +--------------------------------------------+")
		fmt.Printf("  此操作将读取本机 %s 上的所有数据并直接上传!\n", srcDisk.Path)
		if !cli.Confirm("  确认开始传输? 输入 yes 继续") {
			logger.logf("用户取消存储传输")
			fmt.Println("  已取消")
			continue
		}
		logger.logf("用户确认开始存储传输")

		if err := logger.open(filepath.Join(dateDir, base+".log")); err != nil {
			fmt.Printf("  [!] 无法创建日志文件: %v (继续无日志)\n", err)
		}
		execSaveToStorage(srcDisk, runner, cfg, blockSize, doZero, dateDir, logger)
		logger.close()

		if !cli.Confirm("  继续传输其他磁盘? 输入 yes 继续，其他返回主菜单") {
			return
		}
	}
}

// runDirectLocal is the CLI equivalent of runLocalInteractive:
//
//	disk-cloner -l /dev/sda -dst 'sftp://user:pass@nas.lan/backup/' -y
//	disk-cloner -l /dev/sda -o /mnt/usb/backup.img.gz -y
func runDirectLocal(diskPath, bs string, autoYes bool, saveFile, dst string) {
	if err := clone.ValidateDevicePath(diskPath); err != nil {
		log.Fatalf("无效的磁盘路径 %q: %v", diskPath, err)
	}
	if err := clone.ValidateBlockSize(bs); err != nil {
		log.Fatalf("无效的块大小 %q: %v (示例: 4M, 1M, 512K)", bs, err)
	}
	var dstCfg *storage.Config
	if dst != "" {
		cfg, err := storage.ParseURL(dst)
		if err != nil {
			log.Fatalf("%v", err)
		}
		dstCfg = &cfg
	}

	runner := local.NewRunner()

	// The machine being imaged is THIS one — it must be in RAM OS.
	readiness := checkRemoteReadiness(runner, "本机")
	if !confirmUnsafeRemote(readiness, autoYes, "本机") {
		fmt.Println("已取消")
		return
	}

	// Direct mode defaults: rebuild initramfs for cross-hardware boot.
	fixInitramfs = true

	disks, err := disk.GetLocalDisks()
	if err != nil {
		log.Fatalf("本机磁盘扫描失败: %v", err)
	}
	srcDisk := disk.FindDisk(disks, diskPath)
	if srcDisk == nil {
		log.Fatalf("本机磁盘未找到: %s", diskPath)
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "local"
	}
	cliDisk := cli.DiskItem{Path: srcDisk.Path, SizeBytes: srcDisk.SizeBytes, SizeHuman: srcDisk.SizeHuman, Name: srcDisk.Name}
	fmt.Printf("本机: %s (%s)\n", diskPath, srcDisk.SizeHuman)

	dateStr := time.Now().Format("2006-01-02")

	if saveFile != "" {
		if saveFile == "auto" {
			dateDir := filepath.Join(".", dateStr)
			if err := os.MkdirAll(dateDir, 0755); err != nil {
				log.Fatalf("无法创建保存目录 %s: %v", dateDir, err)
			}
			saveFile = filepath.Join(dateDir, makeFileName(host, srcDisk.Name, srcDisk.SizeHuman, dateStr, saveExtFor(compressLevel)))
		}
		if compressLevel == 0 && strings.HasSuffix(saveFile, ".img.gz") {
			saveFile = strings.TrimSuffix(saveFile, ".img.gz") + ".img"
		}
		if compressLevel == 0 {
			fmt.Printf("文件: %s (不压缩)\n", saveFile)
		} else {
			fmt.Printf("文件: %s (gzip)\n", saveFile)
		}

		logger := newSessionLogger()
		logger.logf("=== Disk Cloner v%s 本机模式 (保存文件) ===", version)
		logger.logf("源磁盘: %s (%s)", srcDisk.Path, srcDisk.SizeHuman)
		logger.logf("本机环境: OS=%q RootFS=%q Alpine=%v RAM=%v Detected=%v",
			readiness.OSLine, readiness.RootFS, readiness.IsAlpine, readiness.IsRAM, readiness.Detected)
		logger.logf("保存文件: %s", saveFile)
		logger.logf("块大小: %s  压缩级别: %d  压缩方式: %s  零填充: 是  重建 initramfs: %v",
			bs, compressLevel, compressTypeName(compressType), fixInitramfs)

		if !autoYes {
			fmt.Printf("将保存本机 %s 到文件 %s\n", srcDisk.Path, saveFile)
			if !cli.Confirm("确认继续? 输入 yes") {
				fmt.Println("已取消")
				return
			}
		}

		logPath := saveFile + ".log"
		if err := logger.open(logPath); err != nil {
			fmt.Printf("  [!] 无法创建日志文件 %s: %v (继续无日志)\n", logPath, err)
		} else {
			fmt.Printf("  日志文件: %s\n", logPath)
		}
		execSaveToFile("本机", cliDisk, runner, saveFile, bs, true, logger)
		logger.close()
		return
	}

	// dst != ""
	cfg := *dstCfg
	if cfg.NeedsName() {
		cfg.AppendName(makeFileName(host, srcDisk.Name, srcDisk.SizeHuman, dateStr, saveExtFor(compressLevel)))
	}
	fmt.Printf("目标: %s\n", storage.Describe(cfg))

	logger := newSessionLogger()
	logger.logf("=== Disk Cloner v%s 本机模式 (远程存储) ===", version)
	logger.logf("源磁盘: %s (%s)", srcDisk.Path, srcDisk.SizeHuman)
	logger.logf("本机环境: OS=%q RootFS=%q Alpine=%v RAM=%v Detected=%v",
		readiness.OSLine, readiness.RootFS, readiness.IsAlpine, readiness.IsRAM, readiness.Detected)
	logger.logf("存储目标: %s", storage.Describe(cfg))
	logger.logf("块大小: %s  压缩级别: %d  压缩方式: %s  零填充: 是  重建 initramfs: %v",
		bs, compressLevel, compressTypeName(compressType), fixInitramfs)

	if !autoYes {
		fmt.Printf("将读取本机 %s 并流式上传到 %s\n", srcDisk.Path, storage.Describe(cfg))
		if !cli.Confirm("确认继续? 输入 yes") {
			fmt.Println("已取消")
			return
		}
	}

	dateDir := filepath.Join(".", dateStr)
	if err := os.MkdirAll(dateDir, 0755); err != nil {
		log.Fatalf("无法创建日志目录 %s: %v", dateDir, err)
	}
	logPath := filepath.Join(dateDir, storage.BaseName(cfg)+".log")
	if err := logger.open(logPath); err != nil {
		fmt.Printf("  [!] 无法创建日志文件 %s: %v (继续无日志)\n", logPath, err)
	} else {
		fmt.Printf("  日志文件: %s\n", logPath)
	}
	execSaveToStorage(cliDisk, runner, cfg, bs, true, dateDir, logger)
	logger.close()
}

// askStorageConn prompts for the storage backend and its connection
// details. Returns ok=false when the user goes back (q).
func askStorageConn() (storage.Config, bool) {
	fmt.Println("  存储类型 — 输入序号选择:")
	fmt.Println("  [1] SFTP (SSH 文件传输, 推荐)")
	fmt.Println("  [2] FTP")
	fmt.Println("  [3] WebDAV")
	fmt.Println("  [4] S3 / 对象存储 (AWS S3, MinIO 等 S3 兼容)")
	fmt.Println("  [5] Pixeldrain (网盘直传, 需要 API Key)")
	kind := cli.SelectOption("请输入序号", 1, 5)
	if isBack(kind) {
		return storage.Config{}, false
	}
	cfg := storage.Config{InsecureTLS: !tlsVerifyEnabled}
	switch kind {
	case 1:
		cfg.Kind = storage.KindSFTP
		cfg.Host = cli.ReadInput("存储服务器 IP", "")
		if cfg.Host == "" {
			return cfg, false
		}
		cfg.Host = extractIP(cfg.Host)
		cfg.Port = cli.ReadInt("SSH 端口", 22)
		cfg.User = cli.ReadInput("用户名", "root")
		cfg.Password = cli.ReadPassword("密码 (回车使用密钥)")
	case 2:
		cfg.Kind = storage.KindFTP
		cfg.Host = cli.ReadInput("FTP 服务器 IP", "")
		if cfg.Host == "" {
			return cfg, false
		}
		cfg.Host = extractIP(cfg.Host)
		cfg.Port = cli.ReadInt("端口", 21)
		cfg.User = cli.ReadInput("用户名", "")
		cfg.Password = cli.ReadPassword("密码")
	case 3:
		cfg.Kind = storage.KindWebDAV
		for {
			base := cli.ReadInput("WebDAV 地址 (如 https://nas.lan:5006/dav)", "")
			if base == "" {
				return cfg, false
			}
			if !strings.Contains(base, "://") {
				fmt.Println("  [!] 地址需包含 http:// 或 https:// 前缀")
				continue
			}
			cfg.URL = strings.TrimRight(base, "/") // file name appended later
			break
		}
		cfg.User = cli.ReadInput("用户名", "")
		cfg.Password = cli.ReadPassword("密码")
	case 4:
		cfg.Kind = storage.KindS3
		cfg.Endpoint = cli.ReadInput("Endpoint (如 s3.amazonaws.com 或 minio.lan:9000)", "s3.amazonaws.com")
		tlsInput := strings.ToLower(cli.ReadInput("使用 HTTPS? (回车=是, 输入n=否) [Y/n]", "y"))
		cfg.UseTLS = tlsInput != "n" && tlsInput != "no"
		cfg.Region = cli.ReadInput("Region", "us-east-1")
		cfg.Bucket = cli.ReadInput("Bucket", "")
		if cfg.Bucket == "" {
			return cfg, false
		}
		cfg.PathStyle = strings.HasPrefix(cli.ReadInput("寻址方式 (1=虚拟主机, 2=path-style, MinIO/自建选 2)", "1"), "2")
		cfg.AccessKey = cli.ReadInput("Access Key ID", "")
		cfg.SecretKey = cli.ReadPassword("Secret Access Key")
	case 5:
		cfg.Kind = storage.KindPixelDrain
		cfg.Host = "pixeldrain.com"
		cfg.Port = 443
		cfg.UseTLS = true
		fmt.Println("  API Key 在 pixeldrain.com 账户设置 (Account Settings) 页面获取")
		cfg.Password = cli.ReadPassword("Pixeldrain API Key")
		if cfg.Password == "" {
			fmt.Println("  [!] Pixeldrain 不支持匿名上传,必须提供 API Key")
			return cfg, false
		}
	}
	return cfg, true
}

// fillDestPath asks for the destination file path/URL/key, pre-filled with
// an auto-generated name. Returns false when the user enters nothing.
func fillDestPath(cfg *storage.Config, defaultName string) bool {
	switch cfg.Kind {
	case storage.KindSFTP, storage.KindFTP:
		p := cli.ReadInput("目标路径 (远程文件)", defaultName)
		if p == "" {
			return false
		}
		cfg.Path = p
	case storage.KindWebDAV:
		p := cli.ReadInput("目标 URL (完整文件地址)", cfg.URL+"/"+defaultName)
		if p == "" {
			return false
		}
		cfg.URL = p
	case storage.KindS3:
		k := cli.ReadInput("对象 Key", defaultName)
		if k == "" {
			return false
		}
		cfg.Key = k
	case storage.KindPixelDrain:
		p := cli.ReadInput("文件名 (在 pixeldrain 上显示的名称)", defaultName)
		if p == "" {
			return false
		}
		cfg.Path = p
	}
	return true
}

// askTransferSettings prompts for the settings shared by all save modes and
// stores the compression choices in the package globals. Returns the block
// size and the zero-fill choice.
func askTransferSettings(logger *sessionLogger) (string, bool) {
	blockSize := readBlockSize()
	compressLevel = cli.AskCompressionLevel()
	compressType = cli.AskCompressionType()
	doZero := cli.ConfirmZero()
	fixInitramfs = cli.AskFixInitramfs()
	if logger != nil {
		logger.logf("块大小: %s", blockSize)
		logger.logf("压缩级别: %d  压缩方式: %s", compressLevel, compressTypeName(compressType))
		logger.logf("零填充空闲空间: %v", doZero)
		logger.logf("重建 initramfs: %v", fixInitramfs)
	}
	return blockSize, doZero
}

// execSaveToStorage runs the actual save-to-storage without prompts. The
// image streams straight into the remote storage writer; only the small
// .sha256/.size sidecars (and the .log) are written under sidecarDir.
func execSaveToStorage(srcDisk cli.DiskItem, runner sshclient.Runner,
	cfg storage.Config, blockSize string, doZero bool, sidecarDir string, logger *sessionLogger) {

	desc := storage.Describe(cfg)
	fmt.Println()
	fmt.Printf("  正在连接 %s ...\n", desc)
	fmt.Println()

	totalStart := time.Now()
	logger.logf("开始存储传输 (dd -> 网络 -> %s, 请求压缩: %s)", cfg.Kind, compressTypeName(compressType))
	logger.logf("开始时间: %s", totalStart.Format("2006-01-02 15:04:05"))

	// Surface backend warnings (e.g. the WebDAV spool fallback that buffers
	// the whole image locally) on screen and in the log.
	cfg.Logf = func(format string, args ...interface{}) {
		fmt.Printf("  "+format+"\n", args...)
		logger.logf("  "+format, args...)
	}

	// Fail fast: Open validates credentials, path/bucket and permissions
	// BEFORE the potentially hours-long zero-fill starts.
	w, err := storage.Open(cfg)
	if err != nil {
		logger.logf("存储连接失败: %v", err)
		fmt.Printf("  [!] 存储连接失败: %v\n", err)
		return
	}

	job := clone.New(runner, clone.Params{
		SourcePath:       srcDisk.Path,
		TargetPath:       desc, // display only
		SourceSize:       srcDisk.SizeBytes,
		BlockSize:        blockSize,
		ZeroFill:         doZero,
		CompressionLevel: compressLevel,
		CompressType:     compressType,
		FixInitramfs:     fixInitramfs,
	}, makeProgressFnWithLogger(logger))
	job.SetLogFunc(func(format string, args ...interface{}) {
		msg := fmt.Sprintf(format, args...)
		fmt.Printf(format+"\n", args...)
		logger.logf("  %s", msg)
	})

	runErr := job.RunToStream(w)
	if runErr != nil {
		logger.logf("传输失败: %v", runErr)
		fmt.Printf("\n  传输失败: %v\n", runErr)
		if abErr := w.Abort(); abErr != nil {
			logger.logf("清理远端残留失败: %v", abErr)
		} else {
			fmt.Println("  [!] 远端未完成的上传已清理")
		}
		return
	}
	if err := w.Close(); err != nil {
		logger.logf("上传收尾失败: %v", err)
		fmt.Printf("\n  上传收尾失败: %v\n", err)
		return
	}
	// Pixeldrain 等后端会上报可分享的链接
	if linker, ok := w.(interface{ ShareURL() string }); ok {
		if u := linker.ShareURL(); u != "" {
			logger.logf("分享链接: %s", u)
			fmt.Printf("  分享链接: %s\n", u)
			fmt.Printf("  直链下载: %s?download\n", u)
		}
	}
	logger.logf("传输完成")

	// Sidecars live next to nothing on the remote — write them locally so
	// they can accompany the image when it is downloaded again.
	base := storage.BaseName(cfg)
	if job.ChecksumHex != "" {
		writeChecksumFile(filepath.Join(sidecarDir, base), job.ChecksumHex)
	}
	clone.WriteSizeFile(filepath.Join(sidecarDir, base), srcDisk.SizeBytes)
	logger.logf("校验/大小文件已生成(本地): %s.sha256 / %s.size", base, base)

	fmt.Println()
	fmt.Println("  ===============================================")
	fmt.Printf("  传输完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))
	fmt.Println("  ===============================================")
}

func runRestoreToRemote(ip string, srcDisk cli.DiskItem, sshClient *sshclient.Client) {
	var fileName string
	for {
		input := cli.ReadInputPath("本地文件路径 (Tab=补全, 回车=浏览, q=返回)", "")
		if input == "" && cli.StdinClosed() {
			// Piped/redirected input exhausted — leave the loop instead of
			// spinning forever between EOF and the empty file list.
			fmt.Println("\n  [!] 标准输入已结束, 返回菜单")
			return
		}
		lower := strings.ToLower(input)
		if lower == "q" || lower == "quit" {
			return
		}
		if input == "" {
			fileName = browseLocalFiles()
			if fileName == "" {
				// 用户在文件列表按 q 返回 — 回到手动输入流程，不直接退出
				continue
			}
		} else {
			fileName = input
		}
		if _, err := os.Stat(fileName); err != nil {
			fmt.Printf("\n  [!] 无法访问文件: %s\n      %v\n", fileName, err)
			fmt.Println("  请重新输入路径 (Tab 可补全, 回车浏览列表, 输入 q 返回)")
			continue
		}
		break
	}

	fmt.Println()
	fmt.Printf("  已选中文件: %s\n", fileName)
	fmt.Println()

	remoteDisk := cli.ReadInput("远程目标磁盘，确认请回车", srcDisk.Path)

	// Validate BEFORE the path is embedded in any remote shell command
	// (lsblk size check etc.) — blocks command injection via this input.
	if err := clone.ValidateDevicePath(remoteDisk); err != nil {
		fmt.Printf("  [!] 无效的设备路径: %v\n", err)
		return
	}

	// Show the real size of the target disk (may differ from the source).
	targetLabel := srcDisk.SizeHuman
	if tgtSize, err := getRemoteDiskSize(sshClient, remoteDisk); err == nil && tgtSize > 0 {
		targetLabel = disk.FormatBytes(tgtSize)
	}

	// Pre-flight checks BEFORE the overwrite confirmation, so a corrupt
	// image or a too-small target is caught before the user commits.
	if !verifyChecksum(fileName) {
		if !cli.Confirm("  校验失败，是否继续恢复? 输入 yes 强制恢复") {
			return
		}
	}

	// Check target disk size vs uncompressed image size.
	uncompSize := imageSize(fileName)
	if uncompSize > 0 {
		targetSize, _ := getRemoteDiskSize(sshClient, remoteDisk)
		if targetSize > 0 && uncompSize > targetSize {
			pct := float64(targetSize) / float64(uncompSize) * 100
			fmt.Printf("  [!] 目标盘 (%s) 小于解压后镜像 (%s)，只能写入约 %.1f%%\n",
				disk.FormatBytes(targetSize), disk.FormatBytes(uncompSize), pct)
			if !cli.Confirm("  继续恢复? 输入 yes") {
				return
			}
		}
	}

	fmt.Println()
	fmt.Println("  +--------------------------------------------+")
	fmt.Printf("  |  源文件: %s\n", fileName)
	fmt.Printf("  |  目标:   %s:%s (%s)\n", ip, remoteDisk, targetLabel)
	fmt.Println("  +--------------------------------------------+")
	fmt.Println()
	fmt.Printf("  此操作将覆盖远程 %s 上的所有数据!\n", remoteDisk)
	if !cli.Confirm("  确认开始恢复? 输入 yes 继续") {
		fmt.Println("  已取消")
		return
	}

	fmt.Println()
	fmt.Println("  开始恢复...")
	fmt.Println()

	totalStart := time.Now()
	// Restore consumes no compression/zero-fill/initramfs params — only
	// TargetPath/BlockSize/SourceSize matter here, so leave the rest at
	// zero values rather than passing stale globals.
	job := clone.New(sshClient, clone.Params{
		TargetPath: remoteDisk,
		BlockSize:  "4M",
	}, makeProgressFn())
	job.SetLogFunc(func(format string, args ...interface{}) {
		fmt.Printf(format+"\n", args...)
	})

	if err := job.RestoreFromFile(fileName); err != nil {
		fmt.Printf("\n  恢复失败: %v\n", err)
		return
	}

	// Reinstall GRUB and rebuild initramfs on the freshly restored disk.
	// This is essential: the dd'd image carries the source machine's GRUB
	// core.img (with source-specific embedded block addresses) and source
	// initramfs (with source-only drivers). Without this step the restored
	// disk boots into "GRUB error: unknown filesystem / rescue mode>" or
	// kernel panic "VFS: unable to mount root fs".
	fmt.Println()
	fmt.Println("  修复引导（GRUB + initramfs，确保目标机能启动）...")
	if err := job.FixBoot(remoteDisk); err != nil {
		fmt.Printf("  [!] 引导修复失败: %v（恢复已成功，请手动修复引导）\n", err)
	}

	// Post-restore verification: read-only fsck on all target partitions.
	fmt.Println()
	fmt.Println("  正在验证目标文件系统一致性 (只读 fsck)...")
	if bad := postRestoreFsck(sshClient, remoteDisk); len(bad) > 0 {
		fmt.Printf("  [!] 警告: 以下分区 fsck 报告错误: %s\n", strings.Join(bad, ", "))
		fmt.Println("  [!] 恢复的文件系统可能不一致,重启前请先执行:")
		for _, p := range bad {
			fmt.Printf("        fsck -fy %s\n", p)
		}
	} else {
		fmt.Println("  ✓ 目标文件系统一致性检查通过")
	}

	fmt.Println()
	fmt.Println("  ===============================================")
	fmt.Printf("  恢复完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))
	fmt.Println("  ===============================================")
}

// getRemoteDiskSize returns the size of a disk on the remote server via lsblk.
// stdout is read separately from stderr: merging them (CombinedOutput) lets
// udev/kernel warnings pollute the first line and break the size parse,
// silently disabling the target-too-small safety check.
func getRemoteDiskSize(sshClient *sshclient.Client, dev string) (int64, error) {
	session, err := sshClient.Execute("lsblk -b -n -o SIZE " + shellQuote(dev) + " 2>/dev/null")
	if err != nil {
		return 0, err
	}
	defer session.Close()
	out, _ := io.ReadAll(session.Stdout())
	if err := session.Wait(); err != nil {
		return 0, err
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		return 0, fmt.Errorf("no size returned")
	}
	size, err := strconv.ParseInt(line, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected lsblk output %q: %w", line, err)
	}
	return size, nil
}

// windowsFileDialog opens the native Windows file picker and returns the
// selected file path. Returns empty string if cancelled or unavailable.
func windowsFileDialog() string {
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("diskcloner_%d.txt", time.Now().UnixNano()))
	psCmd := fmt.Sprintf(
		`Add-Type -AssemblyName System.Windows.Forms; $f=New-Object System.Windows.Forms.OpenFileDialog; $f.Filter='Gzip files (*.gz)|*.gz|All files (*.*)|*.*'; $f.Title='Select disk image file'; if($f.ShowDialog() -eq 'OK'){[IO.File]::WriteAllText('%s',$f.FileName,[Text.Encoding]::UTF8)}`,
		tmpFile,
	)
	exec.Command("powershell", "-NoProfile", "-Command", psCmd).Run()

	data, err := os.ReadFile(tmpFile)
	os.Remove(tmpFile)
	if err != nil || len(data) == 0 {
		return ""
	}
	// Strip UTF-8 BOM if present (PowerShell [IO.File]::WriteAllText emits it)
	path := strings.TrimSpace(string(data))
	path = strings.TrimPrefix(path, "\ufeff")
	return path
}

// windowsFolderDialog opens the native Windows folder picker and returns
// the selected directory path. Returns empty string if cancelled.
func windowsFolderDialog() string {
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("diskcloner_%d.txt", time.Now().UnixNano()))
	psCmd := fmt.Sprintf(
		`Add-Type -AssemblyName System.Windows.Forms; $f=New-Object System.Windows.Forms.FolderBrowserDialog; $f.Description='Select save directory'; if($f.ShowDialog() -eq 'OK'){[IO.File]::WriteAllText('%s',$f.SelectedPath,[Text.Encoding]::UTF8)}`,
		tmpFile,
	)
	exec.Command("powershell", "-NoProfile", "-Command", psCmd).Run()

	data, err := os.ReadFile(tmpFile)
	os.Remove(tmpFile)
	if err != nil || len(data) == 0 {
		return ""
	}
	path := strings.TrimSpace(string(data))
	path = strings.TrimPrefix(path, "\ufeff")
	return path
}

func browseLocalFiles() string {
	// On Windows, try the native file open dialog first
	if runtime.GOOS == "windows" {
		if path := windowsFileDialog(); path != "" {
			return path
		}
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		return ""
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".img.gz") {
			files = append(files, e.Name())
		}
	}
	// Also try .gz in case user renamed without .img prefix
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".gz") && !strings.HasSuffix(e.Name(), ".img.gz") {
			files = append(files, e.Name())
		}
	}

	// Raw uncompressed images (saved with compression level 0)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".img") {
			files = append(files, e.Name())
		}
	}

	if len(files) == 0 {
		fmt.Println("\n  当前目录未找到 .img.gz / .img 镜像文件")
		fmt.Println("  在 Windows 上可直接将文件拖拽到 cmd 窗口获取路径")
		return ""
	}

	fmt.Println()
	fmt.Println("  当前目录的镜像文件:")
	for i, f := range files {
		info, _ := os.Stat(f)
		size := ""
		if info != nil {
			size = disk.FormatBytes(info.Size())
		}
		fmt.Printf("  [%d] %-40s %s\n", i+1, f, size)
	}
	idx := cli.SelectDisk("选择文件", 1, len(files))
	if isBack(idx) {
		return ""
	}
	return files[idx-1]
}

// RemoteReadiness holds the result of probing the remote system environment.
// IsAlpineRAM == true means it's safe to clone the system disk directly.
type RemoteReadiness struct {
	OSLine   string
	RootFS   string
	IsAlpine bool
	IsRAM    bool
	// Detected == false means the probe failed (could not read os-release / df),
	// in which case IsAlpine/IsRAM are not reliable and we should be cautious.
	Detected bool
}

// IsSafe returns true only when the remote is detected as Alpine Linux
// running from RAM (tmpfs/overlay/rootfs), which is the only state where
// dd-ing the system disk is guaranteed to produce a consistent image.
func (r RemoteReadiness) IsSafe() bool {
	return r.Detected && r.IsAlpine && r.IsRAM
}

// checkRemoteReadiness probes whether the machine whose disk will be dd'd
// (remote over SSH, or local in 本机模式) runs from RAM. `where` is the
// display label for that machine ("远程" or "本机").
func checkRemoteReadiness(runner sshclient.Runner, where string) RemoteReadiness {
	script := `echo "OS=$(cat /etc/os-release 2>/dev/null | head -1)"
echo "ROOTFS=$(df -T / 2>/dev/null | tail -1 | awk '{print $2}')"`

	// Even on error, CombinedOutput usually returns partial output -
	// parse what we can rather than silently giving up.
	out, err := runner.CombinedOutput(script)

	r := RemoteReadiness{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "OS="):
			r.OSLine = strings.TrimPrefix(line, "OS=")
		case strings.HasPrefix(line, "ROOTFS="):
			r.RootFS = strings.TrimPrefix(line, "ROOTFS=")
		}
	}
	r.IsAlpine = strings.Contains(strings.ToLower(r.OSLine), "alpine")
	r.IsRAM = r.RootFS == "tmpfs" || r.RootFS == "overlay" || r.RootFS == "rootfs"
	r.Detected = r.OSLine != "" || r.RootFS != ""

	fmt.Println()
	if !r.Detected {
		fmt.Printf("  [!] 无法检测%s环境 (os-release 或 df 读取失败)\n", where)
		if err != nil {
			fmt.Printf("      错误: %v\n", err)
		}
		fmt.Printf("      无法确认%s是否处于 Alpine RAM OS,继续可能导致数据不一致\n", where)
	} else if r.IsAlpine && r.IsRAM {
		fmt.Printf("  %s状态: Alpine Linux RAM OS (%s 根文件系统)\n", where, r.RootFS)
		fmt.Println("  磁盘分区未挂载,可以安全克隆。")
	} else if r.IsAlpine && !r.IsRAM {
		fmt.Printf("  [!] %s是 Alpine Linux 但根文件系统不是 tmpfs/overlay\n", where)
		fmt.Printf("      当前根文件系统: %s\n", r.RootFS)
		fmt.Println("      可能是安装到磁盘的 Alpine,继续克隆可能损坏数据!")
	} else {
		fmt.Printf("  [!] %s操作系统: %s\n", where, r.OSLine)
		fmt.Printf("  [!] 根文件系统: %s\n", r.RootFS)
		fmt.Printf("  [!] %s不是 Alpine RAM OS! 如果系统在正常运行,\n", where)
		fmt.Println("      克隆其系统盘可能导致数据不一致。")
		fmt.Println()
		fmt.Printf("  建议先将%s重启进入 Alpine RAM OS 后再克隆。\n", where)
		fmt.Println("  参考 https://github.com/bin456789/reinstall 项目执行 bash reinstall.sh alpine --hold 1")
	}
	return r
}

// confirmUnsafeRemote asks the user to confirm proceeding when the machine
// being imaged is not detected as Alpine RAM OS. Returns true if the user
// explicitly accepts the risk (or if it is safe and no confirmation is
// needed).
func confirmUnsafeRemote(r RemoteReadiness, autoYes bool, where string) bool {
	if r.IsSafe() {
		return true
	}
	if autoYes {
		fmt.Println("  [!] 已使用 -y 跳过确认,继续执行(风险自负)")
		return true
	}
	fmt.Println()
	fmt.Println("  ─────────────────────────────────────────────")
	return cli.Confirm(fmt.Sprintf("  %s不是 Alpine RAM OS,继续可能损坏数据。输入 yes 继续", where))
}

func printFstabWarning(targetDisk string, autoYes bool) {
	warn := func() {
		fmt.Println()
		fmt.Println("  +==============================================")
		fmt.Println("  |  重要提醒 - 务必执行后再重启")
		fmt.Println("  +==============================================")
		fmt.Println("  |")
		fmt.Println("  |  克隆后的磁盘可能因 fstab 中额外的磁盘")
		fmt.Println("  |  挂载配置而导致启动失败 (卡 90 秒超时)。")
		fmt.Println("  |")
		fmt.Printf("  |  请立即挂载目标磁盘并检查 fstab:            \n")
		fmt.Println("  |")
		fmt.Println("  |  1. 创建分区设备节点:                       ")
		fmt.Println("  |     mdev -s                                  ")
		fmt.Println("  |")
		fmt.Println("  |  2. 用 lsblk 查看分区号, 挂载根分区        ")
		fmt.Println("  |     (通常是最大的 ext4/xfs 分区):           ")
		fmt.Printf("  |     lsblk %s\n", targetDisk)
		fmt.Println("  |     mount /dev/<根分区> /mnt                 ")
		fmt.Println("  |")
		fmt.Println("  |  3. 编辑 fstab, 删除或注释掉不存在的设备:   ")
		fmt.Println("  |     vi /mnt/etc/fstab                        ")
		fmt.Println("  |     (注释 /data, /mnt/* 等额外磁盘条目)      ")
		fmt.Println("  |")
		fmt.Println("  |  4. 卸载并重启:                              ")
		fmt.Println("  |     umount /mnt                              ")
		fmt.Println("  |     reboot                                   ")
		fmt.Println("  |")
		fmt.Println("  |  不执行以上操作, 系统可能无法启动!")
		fmt.Println("  +==============================================")
		fmt.Println()
	}
	warn()
	if autoYes {
		// -y means unattended: don't stall the run with the repeated
		// 10-second reminders.
		return
	}
	fmt.Println("  -- 以上提醒将在 10 秒后重复 --")
	time.Sleep(10 * time.Second)
	warn()
	fmt.Println("  -- 最终提醒 --")
	time.Sleep(10 * time.Second)
	warn()
}

func waitExit() {
	fmt.Println()
	fmt.Print("  按回车键退出...")
	fmt.Scanln()
}

func makeFileName(ip, diskName, sizeHuman, dateStr, ext string) string {
	if i := strings.LastIndex(diskName, "/"); i >= 0 {
		diskName = diskName[i+1:]
	}
	// Extract numeric size like "30 GB" -> "30G"
	size := ""
	for _, part := range strings.Fields(sizeHuman) {
		if part != "B" {
			size += part
		}
	}
	if size != "" {
		return fmt.Sprintf("%s-%s-%s-%s%s", ip, diskName, size, dateStr, ext)
	}
	return fmt.Sprintf("%s-%s-%s%s", ip, diskName, dateStr, ext)
}

// saveExtFor returns the image file extension for a compression level:
// level 0 saves an uncompressed raw image (.img), anything else a gzip
// archive (.img.gz).
func saveExtFor(level int) string {
	if level == 0 {
		return ".img"
	}
	return ".img.gz"
}

// extractIP attempts to extract an IPv4 address from user input.
// Handles copied text like "IP: 192.168.1.100" or "192.168.1.100:22".
// Octets are range-checked so garbage like 999.999.999.999 isn't extracted
// as if it were an address; non-matching input (e.g. a hostname) is
// returned as-is and validated at dial time.
var ipRe = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

func extractIP(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	// Try to extract IP from any surrounding text
	if match := ipRe.FindString(input); match != "" && validIPv4(match) {
		return match
	}
	return input
}

// validIPv4 reports whether s is four dot-separated octets in 0-255.
func validIPv4(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n > 255 {
			return false
		}
	}
	return true
}

// sessionLogger buffers timestamped log entries until a target file is opened.
// Used by Mode 2 (save-to-file) to record the full interactive session — from
// SSH connection through readiness check, disk scan, prompts and progress —
// into a .log file written alongside the compressed image.
//
// Entries logged before open() is called are kept in memory; they are flushed
// to the file when open() is invoked (typically after the user confirms the
// save and the final file path is known). If the user cancels, open() is never
// called and the buffered entries are simply discarded when the logger goes
// out of scope — no log file is left behind.
//
// fork() copies the current buffer into a new logger, used by batch mode where
// each disk gets its own log file but should include the common preamble
// (connection / readiness / disk list) captured before the per-disk loop.
type sessionLogger struct {
	mu     sync.Mutex
	file   *os.File
	buffer []string
}

func newSessionLogger() *sessionLogger { return &sessionLogger{} }

// logf appends a timestamped entry. If a file is open it is written immediately,
// otherwise it is buffered in memory until open() flushes the buffer.
func (l *sessionLogger) logf(format string, args ...interface{}) {
	if l == nil {
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05") + "  " + fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		fmt.Fprintln(l.file, line)
	} else {
		l.buffer = append(l.buffer, line)
	}
}

// fork returns a new logger containing a copy of this logger's buffered
// entries. The returned logger is independent (its own buffer/file). Used by
// batch backup where each disk gets its own log file but should include the
// common session preamble captured before the per-disk loop.
func (l *sessionLogger) fork() *sessionLogger {
	if l == nil {
		return newSessionLogger()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	child := &sessionLogger{}
	if len(l.buffer) > 0 {
		child.buffer = make([]string, len(l.buffer))
		copy(child.buffer, l.buffer)
	}
	return child
}

// open creates the log file and flushes any buffered entries to it.
// After open returns successfully, subsequent logf calls write directly
// to the file. Returns an error if the file could not be created.
func (l *sessionLogger) open(path string) error {
	if l == nil {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.file = f
	for _, line := range l.buffer {
		fmt.Fprintln(f, line)
	}
	l.buffer = nil
	return nil
}

// close flushes and closes the underlying file. Safe to call on a logger
// that was never opened (no-op).
func (l *sessionLogger) close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

func makeProgressFn() func(clone.Progress) {
	return makeProgressFnWithLogger(nil)
}

// makeProgressFnWithLogger returns a progress callback that prints to stdout
// and, if logger is non-nil, also writes periodic snapshots to the log file
// (roughly every 10 seconds) plus a final summary on completion.
func makeProgressFnWithLogger(logger *sessionLogger) func(clone.Progress) {
	var last cli.CloneProgress
	var lastLogTime time.Time
	return func(p clone.Progress) {
		if p.Done {
			if p.Error == nil {
				cp := cli.CloneProgress{
					BytesWritten:   p.BytesWritten,
					TotalBytes:     p.TotalBytes,
					Percent:        p.Percent,
					SpeedMBps:      p.SpeedMBps,
					ElapsedSeconds: p.ElapsedSeconds,
					EtaSeconds:     p.EtaSeconds,
				}
				if cp.BytesWritten == 0 {
					cp.BytesWritten = last.BytesWritten
					cp.TotalBytes = last.TotalBytes
					cp.Percent = last.Percent
					cp.SpeedMBps = last.SpeedMBps
					cp.ElapsedSeconds = last.ElapsedSeconds
				}
				cli.PrintProgressComplete(cp)
				if logger != nil {
					logger.logf("传输完成: %s / %s  平均速度 %.1f MB/s  用时 %s",
						disk.FormatBytes(cp.BytesWritten), disk.FormatBytes(cp.TotalBytes),
						cp.SpeedMBps, formatTotalTime(time.Duration(cp.ElapsedSeconds)*time.Second))
				}
			} else {
				if logger != nil {
					if p.Error != nil && strings.Contains(p.Error.Error(), "cancelled") {
						logger.logf("用户取消传输 (已传输 %s)", disk.FormatBytes(last.BytesWritten))
					} else {
						logger.logf("传输出错: %v", p.Error)
					}
				}
			}
			return
		}
		last = cli.CloneProgress{
			BytesWritten:   p.BytesWritten,
			TotalBytes:     p.TotalBytes,
			Percent:        p.Percent,
			SpeedMBps:      p.SpeedMBps,
			ElapsedSeconds: p.ElapsedSeconds,
			EtaSeconds:     p.EtaSeconds,
		}
		cli.PrintProgress(last)

		// Log progress snapshots every ~10 seconds (also log the first one
		// immediately so the user sees that transfer has started).
		if logger != nil && (lastLogTime.IsZero() || time.Since(lastLogTime) >= 10*time.Second) {
			etaStr := "--"
			if p.EtaSeconds > 0 {
				etaStr = formatTotalTime(time.Duration(p.EtaSeconds) * time.Second)
			}
			logger.logf("进度: %s / %s (%.1f%%)  速度 %.1f MB/s  已用 %s  ETA %s",
				disk.FormatBytes(p.BytesWritten), disk.FormatBytes(p.TotalBytes),
				p.Percent, p.SpeedMBps,
				formatTotalTime(time.Duration(p.ElapsedSeconds)*time.Second), etaStr)
			lastLogTime = time.Now()
		}
	}
}

func filterDisks(disks []disk.DiskInfo) []cli.DiskItem {
	var list []cli.DiskItem
	for _, d := range disks {
		if d.Type == "disk" {
			list = append(list, cli.DiskItem{
				Path: d.Path, SizeHuman: d.SizeHuman,
				SizeBytes: d.SizeBytes, Model: d.Model, Name: d.Name,
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

// writeChecksumFile writes a precomputed SHA256 hex digest in the standard
// "sha256sum" text format.
func writeChecksumFile(filePath, hexStr string) {
	os.WriteFile(filePath+".sha256", []byte(fmt.Sprintf("%s  %s\n", hexStr, filepath.Base(filePath))), 0644)
	fmt.Printf("  校验文件: %s.sha256\n", filePath)
}

// saveChecksum computes SHA256 of a file and writes it to filePath+".sha256".
func saveChecksum(filePath string) {
	f, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return
	}

	hash := fmt.Sprintf("%x  %s\n", h.Sum(nil), filepath.Base(filePath))
	os.WriteFile(filePath+".sha256", []byte(hash), 0644)
	fmt.Printf("  校验文件: %s.sha256\n", filePath)
}

// verifyChecksum checks .sha256 file against the actual file. Returns true if valid.
// A missing .sha256 file returns true (nothing to verify against); a present
// but empty/corrupt checksum file, or an unreadable image, returns false —
// "cannot verify" must not be reported as "verified".
func verifyChecksum(filePath string) bool {
	data, err := os.ReadFile(filePath + ".sha256")
	if err != nil {
		return true // no checksum file to verify against
	}

	expected := strings.Fields(string(data))
	if len(expected) == 0 {
		fmt.Println("  [!] .sha256 校验文件为空,无法校验 (按校验失败处理)")
		return false
	}
	if len(expected[0]) != 64 {
		fmt.Printf("  [!] .sha256 校验文件格式无效 (%q),按校验失败处理\n", expected[0])
		return false
	}

	f, err := os.Open(filePath)
	if err != nil {
		fmt.Printf("  [!] 无法读取镜像文件进行校验: %v (按校验失败处理)\n", err)
		return false
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		fmt.Printf("  [!] 读取镜像文件失败: %v (按校验失败处理)\n", err)
		return false
	}

	actual := fmt.Sprintf("%x", h.Sum(nil))
	if actual != expected[0] {
		fmt.Printf("  [!] SHA256 不匹配! 文件可能已损坏\n")
		fmt.Printf("  期望: %s\n", expected[0])
		fmt.Printf("  实际: %s\n", actual)
		return false
	}
	fmt.Println("  ✓ 文件完整性校验通过")
	return true
}

// postRestoreFsck runs a read-only filesystem check on each partition of the
// target disk after a restore. This catches corruption that would otherwise
// only surface as "Journal has aborted" / "bad block bitmap checksum" on boot.
//
// Returns the list of partitions that reported errors (caller may warn).
// Partitions whose filesystem type cannot be identified, or that use a
// filesystem we can't check read-only (xfs has no safe offline check —
// xfs_repair -n requires the filesystem to be unmounted and clean), are
// silently skipped to avoid false positives.
func postRestoreFsck(sshClient *sshclient.Client, targetDisk string) []string {
	// Ensure fsck tools are available on the remote.
	sshClient.CombinedOutput("command -v fsck.ext4 || apk add --quiet e2fsprogs 2>/dev/null")

	// List partitions of the target disk via /sys/block, which doesn't need
	// lsblk and works even on minimal busybox systems.
	diskBase := targetDisk
	if i := strings.LastIndex(targetDisk, "/"); i >= 0 {
		diskBase = targetDisk[i+1:]
	}
	out, _ := sshClient.CombinedOutput(fmt.Sprintf(
		`for p in /sys/block/%s/%s*/partition; do [ -f "$p" ] || continue; `+
			`echo "/dev/$(basename $(dirname "$p"))"; done`,
		diskBase, diskBase))

	var bad []string
	for _, line := range strings.Split(out, "\n") {
		part := strings.TrimSpace(line)
		if part == "" || !strings.HasPrefix(part, "/dev/") {
			continue
		}

		// Detect filesystem type with blkid. Running an ext-family fsck
		// against an xfs partition always fails with "bad magic number in
		// super-block" — that's a tool/FS mismatch, not real corruption.
		fsTypeOut, _ := sshClient.CombinedOutput(fmt.Sprintf(
			`blkid -o value -s TYPE %s 2>/dev/null`, part))
		fsType := strings.TrimSpace(fsTypeOut)

		switch fsType {
		case "ext2", "ext3", "ext4":
			// -n = read-only, don't touch. We just want the exit status.
			if _, err := sshClient.CombinedOutput(fmt.Sprintf("fsck.ext4 -fn %s 2>&1", part)); err != nil {
				bad = append(bad, part)
			}
		case "xfs":
			// xfs_repair -n requires an unmounted, clean FS — too fragile
			// to run automatically post-restore. Skip silently; if xfs is
			// broken the system will fail to boot and the user can run
			// xfs_repair manually from a live OS.
			continue
		case "btrfs":
			// btrfs check is read-only-safe but slow; skip for now.
			continue
		default:
			// Unknown / swap / vfat — skip.
			continue
		}
	}
	return bad
}
