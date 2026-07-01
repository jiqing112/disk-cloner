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
	"time"

	"disk-cloner/internal/cli"
	"disk-cloner/internal/clone"
	"disk-cloner/internal/disk"
	"disk-cloner/internal/fixboot"
	sshclient "disk-cloner/internal/ssh"
)

const (
	remoteLsblkCmd = "lsblk -Jb -o NAME,SIZE,TYPE,MOUNTPOINT,MODEL,SERIAL,TRAN,ROTA,RM,FSTYPE,LABEL"
	clearLine      = "\r                                                                                \r"
	version        = "3.0.0"
)

var compressLevel = 1 // default gzip compression level 1-9, 0 = no compression

func main() {
	var (
		remoteIP   = flag.String("H", "", "远程服务器 IP")
		remotePort = flag.Int("P", 22, "SSH 端口")
		remoteUser = flag.String("u", "root", "SSH 用户名")
		remotePass = flag.String("p", "", "SSH 密码")
		source     = flag.String("s", "", "源磁盘 (远程)")
		target     = flag.String("t", "", "目标磁盘 (本地)")
		bs         = flag.String("bs", "4M", "dd 块大小")
		compressLv = flag.Int("z", 1, "压缩级别 0-9 (0=不压缩, 1=最快, 9=最小)")
		autoYes    = flag.Bool("y", false, "跳过确认")
		saveFile   = flag.String("o", "", "保存为 gzip 文件")
		noFixBoot  = flag.Bool("no-fix-boot", false, "跳过引导修复")
		fixBootDev = flag.String("fix-boot-disk", "", "独立修复引导")
		restoreFile = flag.String("r", "", "恢复 gzip 文件到远程磁盘")
		showVer   = flag.Bool("V", false, "显示版本号")
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

	compressLevel = *compressLv
	if compressLevel < 0 || compressLevel > 9 {
		compressLevel = 1
	}

	if *showVer {
		fmt.Println("Disk Cloner v" + version)
		return
	}

	setupConsole()
	ensureDeps()

	if *fixBootDev != "" {
		fmt.Println()
		fmt.Println("  修复引导 - 独立模式")
		if err := fixboot.Run(fixboot.Config{TargetDisk: *fixBootDev}); err != nil {
			fmt.Printf("\n  修复失败: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *remoteIP != "" && *source != "" && (*target != "" || *saveFile != "" || *restoreFile != "") {
		runDirect(*remoteIP, *remotePort, *remoteUser, *remotePass,
			*source, *target, *bs, *autoYes, *saveFile, *noFixBoot, *restoreFile)
		return
	}

	runInteractive()
}

// setupConsole sets the terminal to UTF-8 mode on Windows,
// so Chinese file paths are read/written correctly.
func setupConsole() {
	if runtime.GOOS != "windows" {
		return
	}
	// Use PowerShell to set console code page to UTF-8 (65001)
	exec.Command("powershell", "-NoProfile", "-Command",
		"$OutputEncoding=[Console]::OutputEncoding=[Text.Encoding]::UTF8; chcp 65001 >$null").Run()
}

func ensureDeps() {
	if runtime.GOOS != "linux" {
		return
	}
	if _, err := exec.LookPath("apk"); err != nil {
		return
	}
	deps := []struct{ pkg, binary string }{
		{"util-linux", "lsblk"},
		{"lvm2", "lvm"},
		{"e2fsprogs", "mkfs.ext4"},
		{"xfsprogs", "mkfs.xfs"},
		{"btrfs-progs", "mkfs.btrfs"},
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func runInteractive() {
	cli.PrintHeader()

	for {
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
		sshClient, err := sshclient.Connect(sshclient.Config{
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
		defer sshClient.Close()
		fmt.Printf(clearLine+"  SSH 连接成功 (%s@%s:%d)\n", user, ip, port)

		fmt.Println()
		fmt.Println("  ─────────────────────────────────────────────")
		checkRemoteReadiness(sshClient)
		ensureRemoteDeps(sshClient)

		fmt.Println()
		fmt.Println("  ─────────────────────────────────────────────")
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
			fmt.Printf(clearLine+"  远程扫描失败: %s\n", msg)
			fmt.Println("    请确认远程已安装 lsblk (apk add util-linux)")
			fmt.Println()
			continue
		}
		remoteDisks, err := disk.ParseJSON(remoteRaw)
		if err != nil {
			fmt.Printf(clearLine+"  解析远程磁盘失败: %v\n", err)
			continue
		}
		fmt.Printf(clearLine+"  发现 %d 块远程磁盘\n", countType(remoteDisks, "disk"))
		remoteList := filterDisks(remoteDisks)
		if len(remoteList) == 0 {
			fmt.Println("\n  远程未发现磁盘设备")
			continue
		}

		fmt.Println()
		cli.PrintSection(fmt.Sprintf("远程磁盘 (%s)", ip))
		cli.PrintDiskList(remoteList, "remote")

		if runtime.GOOS == "windows" {
			fmt.Println()
			fmt.Println("  (Windows 仅支持保存和恢复)")
			fmt.Println("  [2] 保存为压缩文件 (dd -> gzip 文件)")
			fmt.Println("  [3] 恢复文件到远程磁盘 (gzip 文件 -> dd 远程磁盘)")
			fmt.Println("  请输入序号 2 或 3 选择操作模式")
			mode := cli.SelectOption("选择操作模式", 2, 3)

			cli.PrintSection("请选择源磁盘 — 输入序号选定远程磁盘")
			cli.PrintDiskList(remoteList, "remote")
			if mode == 2 {
				fmt.Println("  [0] 备份全部磁盘")
			}
			minIdx := 1
			if mode == 2 {
				minIdx = 0
			}
			idx := cli.SelectDisk("请输入序号", minIdx, len(remoteList))

			if mode == 2 && idx == 0 {
				batchSaveToFile(ip, remoteList, sshClient)
				waitExit()
				return
			}

			disk := remoteList[idx-1]

			if mode == 2 {
				runSaveToFile(ip, disk, sshClient)
			} else {
				runRestoreToRemote(ip, disk, sshClient)
			}
			waitExit()
			return
		}

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
		localList := filterDisks(localDisks)

		if len(localList) > 0 {
			cli.PrintSection("本地磁盘")
			cli.PrintDiskList(localList, "local")
		}

		fmt.Println()
		fmt.Println("  操作模式 — 输入序号选择:")
		fmt.Println("  [1] 克隆到本地磁盘 (dd -> 磁盘)")
		fmt.Println("  [2] 保存为压缩文件 (dd -> gzip 文件)")
		fmt.Println("  [3] 恢复文件到远程磁盘 (gzip 文件 -> dd 远程磁盘)")
		mode := cli.SelectOption("请输入序号", 1, 3)

		fmt.Println()
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

		if mode == 2 && srcIdx == 0 {
			batchSaveToFile(ip, remoteList, sshClient)
			waitExit()
			return
		}

		srcDisk := remoteList[srcIdx-1]

		if mode == 1 {
			if len(localList) == 0 {
				fmt.Println("\n  本地未发现磁盘, 无法克隆")
				fmt.Println("  请重新输入或选择保存为文件模式")
				fmt.Println()
				continue
			}

			cli.PrintSection("选择目标磁盘 — 输入序号选定本地磁盘")
			cli.PrintDiskList(localList, "local")
			tgtIdx := cli.SelectDisk("请输入序号", 1, len(localList))
			tgtDisk := localList[tgtIdx-1]

			fmt.Println()
			fmt.Println("  +--------------------------------------------+")
			fmt.Printf("  |  源:   %s:%s (%s)\n", ip, srcDisk.Path, srcDisk.SizeHuman)
			fmt.Printf("  |  目标: 本地 %s (%s)\n", tgtDisk.Path, tgtDisk.SizeHuman)
			fmt.Println("  +--------------------------------------------+")

			if tgtDisk.SizeBytes < srcDisk.SizeBytes {
				fmt.Printf("\n  警告: 目标盘 (%s) 小于源盘 (%s)\n",
					tgtDisk.SizeHuman, srcDisk.SizeHuman)
			}

			blockSize := cli.ReadInput("块大小", "4M")
			compressLevel = cli.AskCompressionLevel()

			fmt.Println()
			doZero := cli.ConfirmZero()
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
				SourcePath: srcDisk.Path,
				TargetPath: tgtDisk.Path,
				SourceSize: srcDisk.SizeBytes,
				BlockSize:  blockSize,
				ZeroFill:   doZero,
				CompressionLevel: compressLevel,
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
			_ = fixboot.Run

			fmt.Println("  ===============================================")
			fmt.Printf("  全部完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))
			fmt.Println("  ===============================================")

			printFstabWarning(tgtDisk.Path)
			waitExit()
			return
		} else if mode == 2 {
			runSaveToFile(ip, srcDisk, sshClient)
			waitExit()
			return
		} else {
			runRestoreToRemote(ip, srcDisk, sshClient)
			waitExit()
			return
		}
	}
}

func runDirect(ip string, port int, user, pass, source, target, bs string,
	autoYes bool, saveFile string, noFixBoot bool, restoreFile string) {

	sshClient, err := sshclient.Connect(sshclient.Config{
		Host: ip, Port: port, User: user, Password: pass, Timeout: 30,
	})
	if err != nil {
		log.Fatalf("SSH 连接失败: %v", err)
	}
	defer sshClient.Close()

	ensureRemoteDeps(sshClient)

	remoteRaw, err := sshClient.CombinedOutput(remoteLsblkCmd)
	if err != nil || remoteRaw == "" {
		log.Fatalf("远程扫描失败: %v", err)
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
		if saveFile == "auto" {
			dateStr := time.Now().Format("2006-01-02")
			dateDir := filepath.Join(".", dateStr)
			os.MkdirAll(dateDir, 0755)
			saveFile = filepath.Join(dateDir, makeFileName(ip, source, srcDisk.SizeHuman, dateStr))
		}
		fmt.Printf("文件: %s (gzip)\n", saveFile)
		cliDisk := cli.DiskItem{Path: srcDisk.Path, SizeBytes: srcDisk.SizeBytes, SizeHuman: srcDisk.SizeHuman, Name: srcDisk.Name}
		doSaveToFile(ip, cliDisk, sshClient, saveFile)
		return
	}

	if restoreFile != "" {
		if _, err := os.Stat(restoreFile); err != nil {
			log.Fatalf("文件不存在: %s", restoreFile)
		}
		fmt.Printf("文件: %s -> 远程: %s (%s)\n", restoreFile, source, srcDisk.SizeHuman)
		totalStart := time.Now()
		job := clone.New(sshClient, clone.Params{
			TargetPath: source,
			BlockSize:  bs,
			CompressionLevel: compressLevel,
		}, makeProgressFn())
		job.SetLogFunc(func(format string, args ...interface{}) {
			fmt.Printf(format+"\n", args...)
		})
		if err := job.RestoreFromFile(restoreFile); err != nil {
			fmt.Printf("\n恢复失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("恢复完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))
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
		ZeroFill:   true,
		CompressionLevel: compressLevel,
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
	_ = noFixBoot
	printFstabWarning(target)
}

func runSaveToFile(ip string, srcDisk cli.DiskItem, sshClient *sshclient.Client) {
	saveDir := askSaveDirectory()
	dateStr := time.Now().Format("2006-01-02")
	dateDir := filepath.Join(saveDir, dateStr)
	os.MkdirAll(dateDir, 0755)
	fmt.Printf("  保存目录: %s\n", dateDir)

	defaultName := makeFileName(ip, srcDisk.Name, srcDisk.SizeHuman, dateStr)
	fileName := filepath.Join(dateDir, defaultName)
	fileName = cli.ReadInput("文件名", fileName)
	doSaveToFile(ip, srcDisk, sshClient, fileName)
}

// batchSaveToFile saves all remote disks with a single set of prompts.
func batchSaveToFile(ip string, disks []cli.DiskItem, sshClient *sshclient.Client) {
	saveDir := askSaveDirectory()
	dateStr := time.Now().Format("2006-01-02")
	dateDir := filepath.Join(saveDir, dateStr)
	os.MkdirAll(dateDir, 0755)
	fmt.Printf("  保存目录: %s\n", dateDir)

	blockSize := cli.ReadInput("块大小", "4M")
	compressLevel = cli.AskCompressionLevel()
	doZero := cli.ConfirmZero()
	fmt.Println()
	if !cli.Confirm("  确认开始批量备份? 输入 yes 继续") {
		fmt.Println("  已取消")
		return
	}

	for i, d := range disks {
		fmt.Println()
		fmt.Printf("  --- 备份 %d/%d: %s ---\n", i+1, len(disks), d.Path)
		fileName := filepath.Join(dateDir, makeFileName(ip, d.Name, d.SizeHuman, dateStr))
		execSaveToFile(ip, d, sshClient, fileName, blockSize, doZero)
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

func doSaveToFile(ip string, srcDisk cli.DiskItem, sshClient *sshclient.Client, fileName string) {
	fmt.Println()
	fmt.Println("  +--------------------------------------------+")
	fmt.Printf("  |  源:   %s:%s (%s)\n", ip, srcDisk.Path, srcDisk.SizeHuman)
	fmt.Printf("  |  文件: %s\n", fileName)
	fmt.Println("  |  格式: gzip 压缩")
	fmt.Println("  +--------------------------------------------+")
	if runtime.GOOS != "windows" {
		fmt.Println()
		fmt.Println("  注意: 如果在 RAM OS 中运行,")
		fmt.Println("    文件将写入内存文件系统，请确保内存充足")
		fmt.Println("    或将文件保存到已挂载的物理磁盘路径")
	}
	blockSize := cli.ReadInput("块大小", "4M")

	compressLevel = cli.AskCompressionLevel()

	doZero := cli.ConfirmZero()
	fmt.Println()
	if !cli.Confirm("  确认开始保存? 输入 yes 继续") {
		fmt.Println("  已取消")
		return
	}
	execSaveToFile(ip, srcDisk, sshClient, fileName, blockSize, doZero)
}

// execSaveToFile runs the actual save without prompts.
func execSaveToFile(ip string, srcDisk cli.DiskItem, sshClient *sshclient.Client, fileName string, blockSize string, doZero bool) {
	fmt.Println()
	fmt.Println("  开始保存...")
	fmt.Println()

	totalStart := time.Now()
	job := clone.New(sshClient, clone.Params{
		SourcePath: srcDisk.Path,
		TargetPath: fileName,
		SourceSize: srcDisk.SizeBytes,
		BlockSize:  blockSize,
		ZeroFill:   doZero,
		CompressionLevel: compressLevel,
	}, makeProgressFn())
	job.SetLogFunc(func(format string, args ...interface{}) {
		fmt.Printf(format+"\n", args...)
	})
	if err := job.RunToFile(); err != nil {
		fmt.Printf("\n  保存失败: %v\n", err)
		return
	}

	saveChecksum(fileName)

	if info, err := os.Stat(fileName); err == nil {
		ratio := 0.0
		if srcDisk.SizeBytes > 0 {
			ratio = float64(info.Size()) / float64(srcDisk.SizeBytes) * 100
		}
		fmt.Printf("  文件大小: %s (压缩率 %.1f%%)\n", disk.FormatBytes(info.Size()), ratio)
	}
	fmt.Println()
	fmt.Println("  ===============================================")
	fmt.Printf("  保存完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))
	fmt.Println("  ===============================================")
}

func runRestoreToRemote(ip string, srcDisk cli.DiskItem, sshClient *sshclient.Client) {
	fileName := cli.ReadInputPath("本地文件 (.img.gz, 回车浏览)", "")

	if fileName == "" {
		fileName = browseLocalFiles()
		if fileName == "" {
			return
		}
	}

	if _, err := os.Stat(fileName); err != nil {
		fmt.Printf("\n  文件不存在: %s\n", fileName)
		return
	}

	fmt.Println()
	fmt.Printf("  已选中文件: %s\n", fileName)
	fmt.Println()

	remoteDisk := cli.ReadInput("远程目标磁盘，确认请回车", srcDisk.Path)

	fmt.Println()
	fmt.Println("  +--------------------------------------------+")
	fmt.Printf("  |  源文件: %s\n", fileName)
	fmt.Printf("  |  目标:   %s:%s (%s)\n", ip, remoteDisk, srcDisk.SizeHuman)
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

	// Check target disk size vs uncompressed image size
	uncompSize := clone.GzipUncompressedSize(fileName)
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

	totalStart := time.Now()
	job := clone.New(sshClient, clone.Params{
		TargetPath: remoteDisk,
		BlockSize:  "4M",
		CompressionLevel: compressLevel,
	}, makeProgressFn())
	job.SetLogFunc(func(format string, args ...interface{}) {
		fmt.Printf(format+"\n", args...)
	})

	if !verifyChecksum(fileName) {
		if !cli.Confirm("  校验失败，是否继续恢复? 输入 yes 强制恢复") {
			return
		}
	}

	if err := job.RestoreFromFile(fileName); err != nil {
		fmt.Printf("\n  恢复失败: %v\n", err)
		return
	}

	fmt.Println()
	fmt.Println("  ===============================================")
	fmt.Printf("  恢复完成! 总耗时: %s\n", formatTotalTime(time.Since(totalStart)))
	fmt.Println("  ===============================================")
}

// getRemoteDiskSize returns the size of a disk on the remote server via lsblk.
func getRemoteDiskSize(sshClient *sshclient.Client, disk string) (int64, error) {
	out, err := sshClient.CombinedOutput(fmt.Sprintf("lsblk -b -n -o SIZE %s 2>/dev/null | head -1", disk))
	if err != nil {
		return 0, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return 0, fmt.Errorf("no size returned")
	}
	size, err := strconv.ParseInt(out, 10, 64)
	return size, err
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

	if len(files) == 0 {
		fmt.Println("\n  当前目录未找到 .img.gz 文件")
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
	return files[idx-1]
}

func checkRemoteReadiness(sshClient *sshclient.Client) {
	script := `echo "OS=$(cat /etc/os-release 2>/dev/null | head -1)"
echo "ROOTFS=$(df -T / 2>/dev/null | tail -1 | awk '{print $2}')"`
	out, err := sshClient.CombinedOutput(script)
	if err != nil {
		return
	}
	osLine, rootFS := "", ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "OS="):
			osLine = strings.TrimPrefix(line, "OS=")
		case strings.HasPrefix(line, "ROOTFS="):
			rootFS = strings.TrimPrefix(line, "ROOTFS=")
		}
	}
	isAlpine := strings.Contains(strings.ToLower(osLine), "alpine")
	isRAM := rootFS == "tmpfs" || rootFS == "overlay" || rootFS == "rootfs"
	fmt.Println()
	if isAlpine && isRAM {
		fmt.Printf("  远程状态: Alpine Linux RAM OS (%s 根文件系统)\n", rootFS)
		fmt.Println("  磁盘分区未挂载，可以安全克隆。")
	} else if isAlpine && !isRAM {
		fmt.Println("  远程是 Alpine Linux 但根文件系统不是 tmpfs/overlay")
		fmt.Printf("  (当前根文件系统: %s)\n", rootFS)
		fmt.Println("  可能是安装到磁盘的 Alpine，继续克隆可能损坏数据！")
	} else {
		fmt.Printf("  远程操作系统: %s\n", osLine)
		fmt.Printf("  根文件系统: %s\n", rootFS)
		fmt.Println("  远程不是 Alpine RAM OS！如果远程系统在正常运行,")
		fmt.Println("  克隆其系统盘可能导致数据不一致。")
		fmt.Println()
		fmt.Println("  建议先将远程服务器重启进入 Alpine RAM OS 后再克隆。")
		fmt.Println("  参考 https://github.com/bin456789/reinstall 项目执行 bash reinstall.sh alpine --hold 1")
	}
}

func printFstabWarning(targetDisk string) {
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
		fmt.Println("  |  2. 用 lsblk 查看分区号, 挂载根分区:        ")
		fmt.Printf("  |     mount %s4 /mnt                        \n", targetDisk)
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
	fmt.Println("  -- 以上提醒将在 10 秒后重复 --")
	time.Sleep(10 * time.Second)
	warn()
	fmt.Println("  -- 最终提醒 --")
	time.Sleep(10 * time.Second)
	warn()
}

func waitExit() {
	if runtime.GOOS == "windows" {
		fmt.Println()
		fmt.Print("  按回车键退出...")
		fmt.Scanln()
	}
}

func makeFileName(ip, diskName, sizeHuman, dateStr string) string {
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
		return fmt.Sprintf("%s-%s-%s-%s.img.gz", ip, diskName, size, dateStr)
	}
	return fmt.Sprintf("%s-%s-%s.img.gz", ip, diskName, dateStr)
}

// extractIP attempts to extract an IPv4 address from user input.
// Handles copied text like "IP: 192.168.1.100" or "192.168.1.100:22".
var ipRe = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

func extractIP(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	// Try to extract IP from any surrounding text
	match := ipRe.FindString(input)
	if match != "" {
		return match
	}
	return input
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
func verifyChecksum(filePath string) bool {
	data, err := os.ReadFile(filePath + ".sha256")
	if err != nil {
		return true // no checksum file to verify against
	}

	f, err := os.Open(filePath)
	if err != nil {
		return true
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return true
	}

	actual := fmt.Sprintf("%x", h.Sum(nil))
	expected := strings.Fields(string(data))
	if len(expected) == 0 {
		return true
	}

	if actual != expected[0] {
		fmt.Printf("  [!] SHA256 不匹配! 文件可能已损坏\n")
		fmt.Printf("  期望: %s\n", expected[0])
		fmt.Printf("  实际: %s\n", actual)
		return false
	}
	fmt.Println("  ✓ 文件完整性校验通过")
	return true
}
