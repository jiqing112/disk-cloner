# Disk Cloner

通过 SSH 远程克隆、备份、恢复整块硬盘的 Go 工具，也可把整盘镜像直传远程存储（SFTP / FTP / WebDAV / S3 / Pixeldrain）。

单个可执行文件即拷即用，支持 Linux 和 Windows，SSH 和 gzip 均已内置，无需安装任何依赖。全程交互式菜单：选磁盘、设参数都是选择题，不需要记命令。

## 功能一览

| 模式 | 数据流 | 典型用途 |
|------|--------|---------|
| 1 克隆 | 远程整盘 → 本地整盘 | 服务器迁移、两台对拷 |
| 2 备份 | 远程整盘 → 本地 `.img.gz`（单盘或全部） | 系统备份、镜像存档 |
| 3 恢复 | 本地 `.img.gz` → 远程整盘 | 系统还原、批量部署 |
| 4 传输 | 本机整盘 → 远程存储 | 异地备份，镜像不占本地空间 |

- 备份产出标准 gzip 镜像，可脱离本工具用 `gunzip` + `dd` 恢复
- 克隆/恢复完成后自动重装 GRUB + 重建 initramfs，通常直接重启即可启动
- 传输过程实时显示进度、速度、ETA，自动 SHA256 校验

## 快速开始

### 第 1 步：让服务器进入 Alpine RAM OS

dd 操作的是整块物理硬盘（含分区表、引导扇区），系统运行中无法安全读写，需要先进入内存系统。在要操作的服务器上执行：

```bash
# 国外服务器
curl -O https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh

# 国内服务器
curl -O https://cnb.cool/bin456789/reinstall/-/git/raw/main/reinstall.sh

# 重启进入 Alpine 内存系统
bash reinstall.sh alpine --hold 1
```

重启完成后 SSH 登录：端口 22，用户 root，密码在脚本执行时终端显示。

哪些机器需要进 RAM OS：

| 模式 | 源服务器（被读的） | 目标服务器（被写的） |
|------|------------------|---------------------|
| 1 克隆 | ✅ 必须 | ✅ 必须 |
| 2 备份 | ✅ 必须 | 不需要（本地接收文件） |
| 3 恢复 | 不需要（本地发送文件） | ✅ 必须 |
| 4 传输 | ✅ 备份系统盘时必须 | 不需要（存储服务在线即可） |

> 在 Windows 上运行程序做备份/恢复时，Windows 端不需要进 RAM OS（Windows 不做 dd 操作）。

> **目标盘容量必须 ≥ 源盘**。整盘 dd 不会自动调整分区，目标盘偏小会截断文件系统导致损坏。

### 第 2 步：下载并运行程序

从 [Releases](https://github.com/jiqing112/disk-cloner/releases) 下载对应平台的可执行文件：

```bash
# Linux
chmod +x disk-cloner-linux-amd64
./disk-cloner-linux-amd64

# Windows
disk-cloner-windows-amd64.exe
```

### 第 3 步：按菜单操作

```
  操作模式 — 输入序号选择:
  [1] 克隆到本地磁盘 (dd -> 磁盘)
  [2] 保存为压缩文件 (dd -> gzip 文件)
  [3] 恢复文件到远程磁盘 (gzip 文件 -> dd 远程磁盘)
  [4] 读取本地硬盘传输到远程存储 (dd -> SFTP/FTP/WebDAV/S3)
```

Windows 上只显示 [2] 和 [3]（克隆和本机传输需要 Linux 块设备）。

执行中会看到这样的进度条（每秒刷新，30 秒滑动窗口计算平均速度和 ETA）：

```
  [================>-----------------------]  45.2%  118.5 MB/s  18.1GB/40.0GB  用时: 2分35秒  ETA: 2分55秒
```

---

## 模式 1 — 克隆：远程盘 → 本地盘

把服务器 A 的整块硬盘原样复制到服务器 B 的硬盘。**程序在目标机 B 上运行，A、B 都要进 RAM OS。**数据在 A 上压缩后经 SSH 传输，B 解压写入，程序跑在 B 的内存里，不会被 dd 覆盖。

```bash
# 1. 上传程序到目标机 B
scp disk-cloner-linux-amd64 root@B_IP:/tmp/

# 2. 在 B 上运行
ssh root@B_IP
cd /tmp && chmod +x disk-cloner-linux-amd64 && ./disk-cloner-linux-amd64
```

然后按菜单提示：选 `[1]` → 输入 A 的 SSH 信息 → 选 A 上的源盘 → 选 B 上的本地目标盘 → 设置参数 → 输入 `yes` 确认开始。

- 目标盘小于源盘时会警告并要求输入 `yes` 强制继续（会截断数据，正常应换更大的盘）
- 完成后自动重装 GRUB + 重建 initramfs + 修 fstab，通常直接重启即可启动

## 模式 2 — 备份：远程盘 → 本地镜像文件

把远程服务器整盘保存为本地压缩镜像。**只需源服务器进 RAM OS，程序在本地运行（Linux / Windows 均可）。**

运行程序选 `[2]` → 输入远程 SSH 信息 → 选磁盘（选 `[0]` 可一次备份全部磁盘，共享同一组设置逐个执行）→ 选保存目录 → 确认文件名 → `yes` 开始。

- 保存目录回车 = 当前目录下自动创建日期子目录（如 `2026-07-03/`）；Windows 输入 `b` 弹出文件夹浏览对话框
- 默认文件名 `IP-磁盘名-容量-日期.img.gz`，每个镜像附带三个小文件：`.sha256` 校验、`.size` 解压后真实大小、`.log` 传输日志
- 传输全程先写 `.partial` 临时文件，成功后才改名为正式文件——中途中断不会破坏已有的旧备份，残留的 `.partial` 也不会被误当成有效备份
- `-z 0` 不压缩时输出原始 `.img` 文件，恢复时程序自动识别

镜像是标准 gzip 格式，可脱离本工具恢复：

```bash
gunzip -c 192.168.1.100-sda-30G-2026-07-03.img.gz | dd of=/dev/sda bs=4M
sha256sum -c 192.168.1.100-sda-30G-2026-07-03.img.gz.sha256
```

## 模式 3 — 恢复：本地镜像文件 → 远程盘

把备份镜像写回远程服务器硬盘。**只需目标服务器进 RAM OS，程序在本地运行（Linux / Windows 均可）。**

运行程序选 `[3]` → 输入目标机 SSH 信息 → 选择本地镜像文件 → 确认远程目标盘 → `yes` 开始。

- 文件选择：直接输入路径（Linux 支持 Tab 补全）、回车浏览目录（Windows 弹出文件对话框）、拖拽文件到 cmd 窗口（Windows）
- 存在 `.sha256` 时自动校验完整性；自动对比解压后大小与目标盘容量，目标盘过小时警告
- 完成后自动重装 GRUB + 重建 initramfs + 修 fstab + 只读 fsck 一致性检查，通常直接重启即可启动

## 模式 4 — 传输：本机盘 → 远程存储

把本机硬盘整盘直传远程存储，**镜像全程不占本地磁盘空间**。程序直接运行在源服务器上（推荐已进 RAM OS），连 SSH 都不需要；本机硬盘只读，数据在内存中压缩后即刻上传。仅支持 Linux。

```bash
# 交互模式：运行程序选 [4]，按提示选本机磁盘、选存储类型、填账号
# 命令行模式：
./disk-cloner -l /dev/sda -dst 'sftp://user:pass@nas.lan/backup/' -y
./disk-cloner -l /dev/sda -dst 's3://AKID:SECRET@minio.lan:9000/bkt/img.gz?path=1' -y
./disk-cloner -l /dev/sda -dst 'pixeldrain://:APIKEY@pixeldrain.com/backup.img.gz' -y
```

| 存储类型 | 说明 |
|---------|------|
| SFTP | 推荐。密码或密钥认证，自动创建缺失目录 |
| FTP | 被动模式（EPSV/PASV），自动建目录 |
| WebDAV | 自动探测分块上传；服务器不支持时（如 nginx dav）回退本地暂存再 PUT |
| S3 | AWS S3 及 MinIO/Ceph 等兼容存储，分片上传（大文件自动加大分片），支持虚拟主机/path-style |
| Pixeldrain | 公共网盘直传，传完即得分享链接，适合没有自己存储服务器的场景 |

要点：

- **连接校验在零填充之前**：账号密码错误、路径/桶不可写会在几秒内报错，不会白跑几小时零填充才发现存不了
- **失败自动清理**：传输中断时删除远端半截文件（SFTP/FTP/WebDAV）、中止 S3 分片任务；本地只留下 `.sha256`/`.size`/`.log` 校验文件
- 高级用法：也可在一台中转机上 `-H IP -p 密码 -s /dev/sda -dst ...`，通过 SSH 读远端磁盘后转发给存储，适合脚本化批量备份
- `-l` 也可搭配 `-o`，把本机盘保存为本地文件

**Pixeldrain 注意事项**（URL 中 API Key 填在密码位，`pd://` 是别名；API Key 在 pixeldrain.com 账户设置页生成，不支持匿名上传）：

| 事项 | 说明 |
|------|------|
| 单文件大小 | 免费账户约 20 GB（以官方为准）。**超限要等整个文件传完才报错**，上传前先确认压缩后大小在限额内 |
| 保留策略 | 60 天无人访问会被删除（被下载则重新计时），适合中转不适合长期备份 |
| 隐私 | 拿到链接的人都能下载，无密码保护。整盘镜像含服务器全部数据，敏感镜像勿直传（本工具不含加密功能） |
| 断点续传 | 无。中断后只能重新上传 |

---

## 通用选项

模式 1 / 2 / 4 执行前会询问以下参数，**回车即用默认值**：

| 选项 | 默认 | 说明 |
|------|------|------|
| 块大小 | 4M | dd 每次读写的数据量，一般不用改 |
| 压缩级别 | 1 | 0=不压缩（局域网最快）/ 1=最快（默认）/ 6=均衡 / 9=最小（费 CPU） |
| 压缩方式 | gzip | gzip 单核、兼容性最广；pigz 多核并行快 3-4 倍（按需自动安装，失败自动回退 gzip），两者产出格式相同 |
| 零填充 | Y | dd 前把空闲空间写零，大幅提高压缩率（见下） |
| 重建 initramfs | Y | 让镜像包含全部硬件驱动（virtio/nvme/SCSI 等），恢复到不同硬件也能启动 |

**零填充效果**：100 GB 盘、实际数据 14 GB 时，不填充压缩包约 50-90 GB（空闲空间残留数据压不动），填充后约 14 GB。支持 LVM 逻辑卷（自动激活卷组）。磁盘 IO 慢、局域网带宽又够时可以跳过（输 `n`）直接传原始数据。

**交互小技巧**：

- 任何选择菜单输入 `q` 返回上一步
- 一项操作完成后输入 `yes` 继续下一项（SSH 连接保持，可换操作模式）
- 粘贴含多余文字的 IP（如 `IP: 192.168.1.100:22`）会自动提取其中的地址

## 命令行模式

无需交互，直接参数执行：

```bash
# 克隆远程磁盘到本地
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -t /dev/sda -y

# 备份为文件（自动命名 + 日期目录）
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -o auto -y

# 最高压缩 / 不压缩（局域网）
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -o auto -z 9 -y
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -o auto -z 0 -y

# 恢复文件到远程磁盘
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -r backup.img.gz -y

# 本机盘直传远程存储（在源机 RAM OS 上运行）
disk-cloner -l /dev/sda -dst 'sftp://user:pass@nas.lan/backup/' -y

# 中转机模式：SSH 读远端盘转发给存储（脚本化批量备份）
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -dst 'sftp://user:pass@nas.lan/backup/' -y

# 独立修复引导（目标机已进 RAM OS）
./disk-cloner-linux-amd64 --fix-boot-disk /dev/sda
```

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-H` | 远程服务器 IP | — |
| `-P` | SSH 端口 | 22 |
| `-u` | SSH 用户名 | root |
| `-p` | SSH 密码。不提供则用密钥认证；也可用环境变量 `DISK_CLONER_PASSWORD`（避免进 shell 历史） | — |
| `-s` | 源磁盘路径（远程），如 `/dev/sda` | — |
| `-t` | 目标磁盘路径（本地） | — |
| `-o` | 保存为本地镜像文件，`auto` = 自动命名 + 日期目录 | — |
| `-r` | 从本地镜像文件恢复到远程磁盘 | — |
| `-dst` | 传输到远程存储：`sftp://` `ftp://` `dav://` `davs://` `s3://` `pixeldrain://`（别名 `pd://`） | — |
| `-l` | 本机磁盘作为源（程序运行在源机 RAM OS，仅 Linux；搭配 `-dst` 或 `-o`） | — |
| `-bs` | dd 块大小 | 4M |
| `-z` | 压缩级别 0-9 | 1 |
| `-y` | 跳过确认提示 | false |
| `-V` | 显示版本号 | — |
| `--fix-boot-disk <磁盘>` | 独立修复引导 | — |
| `--no-fix-boot` | 命令行克隆（`-t`）和恢复（`-r`）时跳过自动引导修复 | — |
| `-no-zerofill` | 命令行模式跳过零填充（命令行模式默认执行零填充，交互模式会询问） | false |
| `-no-fix-initramfs` | 命令行模式跳过备份前重建 initramfs（命令行模式默认执行，交互模式会询问） | false |
| `-tls-verify` | 对 WebDAV/S3/Pixeldrain 启用 HTTPS 证书校验（也可在 URL 加 `tlsverify=1`） | false |

密码含 `$` `~` `?` `%` 等特殊字符时用单引号包裹：`-p 'kI$~4)Tz?%E5ai78'`。

## 引导修复（GRUB + initramfs）

整盘 dd 后目标盘几乎总要修引导才能启动，常见症状：`grub rescue>`（GRUB 读不到分区）或 `VFS: Unable to mount root fs`（内核缺磁盘驱动）。原因：镜像里的 GRUB 引导代码按源盘几何参数硬编码块偏移，initramfs 默认只打包源机硬件的驱动，换盘/换硬件就失配。

**模式 1 / 模式 3 完成后自动修复，无需手动操作**。流程：扫描分区和 LVM → 定位根分区（ext4/xfs/btrfs）→ chroot 后按发行版重建 initramfs（dracut / update-initramfs / mkinitcpio）→ 重装 GRUB（自动区分 BIOS/UEFI）→ 注释 fstab 中目标机上不存在的磁盘挂载（备份为 `fstab.bak`）。

> 模式 2 备份前的"重建 initramfs"选项只重建 initramfs（在源机执行），不重装 GRUB——GRUB 要装到目标盘，而备份时还没有目标盘。

**盘已经坏了（停在 grub rescue）不用重新 dd**，进 RAM OS 后直接修：

```bash
./disk-cloner-linux-amd64 --fix-boot-disk /dev/sda
```

**启动模式必须一致**：源机和目标机的 BIOS/UEFI 要相同，不一致时修了 GRUB 也起不来。`ls /sys/firmware/efi` 有目录 = UEFI，报 No such file = BIOS。各平台对应设置：VMware（Firmware = EFI）、Hyper-V（Generation 2）、VirtualBox（Enable EFI）、PVE/KVM（OVMF）。

**跨虚拟化平台恢复注意驱动问题**：云服务器一般是 KVM，桌面虚拟机常见 VMware / VirtualBox。实测 Linux 从 KVM 迁移到 VMware Workstation 可以开机，但开机可能卡一会儿，进入系统后记得更新一下内核；而 KVM 里的 Windows 迁到 VMware，常见因硬盘驱动不同导致无法开机。这**不是程序的问题**——镜像恢复只是原样搬运数据，跨虚拟化平台启动需要目标环境有对应的磁盘驱动，需在相同的虚拟化环境才能正常开启。

## 克隆/恢复后的手动收尾

自动引导修复覆盖了绝大多数情况，以下三种需要手动处理：

**1. 目标盘比源盘大 → 扩分区用上剩余空间**（30G 镜像恢复到 1T 盘）：

```bash
growpart /dev/sda 1
xfs_growfs /            # xfs（CentOS 默认）；ext4 用 resize2fs /dev/sda1
df -h /
```

**2. 目标盘比源盘小**（程序会警告，仅当实际数据量装得下才继续）：

```bash
sgdisk -e /dev/sda && partprobe /dev/sda   # 修复被截断的 GPT 备份分区表
xfs_repair /dev/sda1                        # xfs；ext4 用 e2fsck -fy /dev/sda1
```

**3. 重启后卡 90 秒进 emergency mode**（fstab 还挂着目标机上没有的盘，自动修复没注释干净时）：

```bash
mount /dev/sda1 /mnt && vi /mnt/etc/fstab   # 注释掉不存在的磁盘条目（原文件已备份 fstab.bak）
umount /mnt && reboot
```

## 常见问题

**恢复后不能启动？**
看恢复日志末尾有没有 `修复引导`。新版模式 1/3 会自动修复；旧版本或修复失败用 `--fix-boot-disk` 单独修；还不行检查 BIOS/UEFI 是否与源机一致，以及是否跨了虚拟化平台（KVM ↔ VMware/VirtualBox 的驱动问题见[引导修复](#引导修复grub--initramfs)一节）。

**恢复后 fsck 报 `bad magic number in super-block`？**
误报。旧版对所有分区硬跑 `fsck.ext4`，遇到 xfs/btrfs 必然报错，不是真损坏。新版用 blkid 识别类型，xfs/btrfs 自动跳过。担心的话 `xfs_repair -n /dev/sda1` 只读验证。

**压缩包太大？**
依次确认：零填充选了 Y → 压缩方式选 pigz → 级别提到 `-z 6` 或 `-z 9`。

**零填充太慢？**
零填充速度由磁盘 IO 决定。局域网带宽足够时可以跳过（输 `n`），直接传原始数据，不填充压缩率差但不等填充时间。

**SSH 连不上？**
确认 IP/端口/密码正确；确认远程已进入 Alpine RAM OS；密码特殊字符用单引号。程序有密码时不加载本地 SSH 密钥，避免超出服务器 MaxAuthTries。

**主机密钥安全吗？**
程序不校验主机密钥（RAM OS 每次重启重新生成密钥，固定指纹必报错），作为补偿连接后显示主机密钥 SHA256 指纹供人工核对。请勿在不可信网络（公共 Wi-Fi 等）使用。

**WebDAV/S3/Pixeldrain 的 HTTPS 证书校验？**
默认关闭（自建 NAS/MinIO 普遍用自签名证书，开了会全连不上）。不可信网络中使用时加 `-tls-verify` 或 URL 参数 `tlsverify=1` 开启。

**模式 4 存储类型怎么选？**
有 NAS/Linux 服务器 → SFTP（推荐）；有对象存储（AWS/MinIO/Ceph）→ S3；NAS 开了 WebDAV（群晖/坚果云等）→ WebDAV；什么都没有或想把镜像发给别人 → Pixeldrain（注意上面的限制）。

**远程磁盘名不是 sda？**
程序自动扫描列出所有磁盘（vda、nvme0n1 等），选序号即可。恢复后磁盘名变化（vda → sda）是正常的，引导修复会按目标盘名重装 GRUB。

## 平台说明

**Linux**：四种模式全支持。程序会自动在远程安装所需依赖（util-linux、gzip 等，pigz 按需）。文件路径输入支持 Tab 补全，退格/Ctrl+U/Ctrl+W 完整支持。

**Windows**：只支持模式 2 备份和模式 3 恢复。无需安装 SSH 或 gzip；保存时回车弹文件夹对话框、恢复时回车弹文件对话框，支持拖拽文件到窗口；密码输入不回显；中文路径正常（自动 UTF-8）；最小化到任务栏不影响传输；结束前按回车退出，不闪退。

## 自行编译与发布

```bash
git clone https://github.com/jiqing112/disk-cloner
cd disk-cloner

# Linux
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o disk-cloner-linux-amd64 .

# Windows
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o disk-cloner-windows-amd64.exe .
```

Windows 下也可运行 `build.bat`。推送版本标签（`git tag v1.0.0 && git push origin v1.0.0`）触发 GitHub Actions 自动编译发布 Release；双击 `push.bat` 快速提交代码。

## License

MIT
