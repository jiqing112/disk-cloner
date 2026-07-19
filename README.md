# Disk Cloner

通过 SSH 远程克隆/备份/恢复磁盘的 Go 工具。纯 Go 实现，单文件零依赖，支持 Linux 和 Windows。

---

## 功能一览

| 模式 | 方向 | 用途 |
|------|------|------|
| 克隆 | 远程 → 本地磁盘 | 服务器迁移、对拷 |
| 保存 | 远程 → 本地文件（单盘或全部） | 系统备份、镜像存档 |
| 恢复 | 本地文件 → 远程磁盘 | 系统还原、批量部署 |

---

## 前置准备：进入 Alpine RAM OS

### 为什么需要 Alpine RAM OS？

dd 操作的是**整个物理硬盘**（包括分区表、引导扇区、所有分区数据）。如果系统正在运行，文件系统已挂载，dd 读写同时进行会导致数据不一致。Alpine RAM OS 把系统跑在内存里，物理磁盘完全未挂载，可以安全地读写整盘。

### 操作步骤

在需要操作的服务器上，使用 [bin456789/reinstall](https://github.com/bin456789/reinstall#%E5%8A%8F%E8%83%BD-3-%E9%87%8D%E5%90%AF%E5%88%B0--alpine-live-os%E5%86%85%E5%AD%98%E7%B3%BB%E7%BB%9F) 脚本进入 Alpine RAM OS：

```bash
# 下载脚本
curl -O https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh
# 或者国内服务器
curl -O https://cnb.cool/bin456789/reinstall/-/git/raw/main/reinstall.sh

# 执行进入 Alpine RAM OS
bash reinstall.sh alpine --hold 1
```

执行后服务器会重启进入 Alpine Linux 内存系统。重启后 SSH 登录信息：
- 端口：22
- 用户：root
- 密码：脚本执行时会显示（通常是 `[!] root password: xxxxx`）

### 各模式需要哪些服务器进入 RAM OS？

| 模式 | 源服务器（被克隆） | 目标服务器（接收端） |
|------|-------------------|---------------------|
| 模式 1 克隆 | ✅ 必须进入 RAM OS | ✅ 必须进入 RAM OS |
| 模式 2 保存 | ✅ 必须进入 RAM OS | 不需要（本地接收文件） |
| 模式 3 恢复 | 不需要（本地发送文件） | ✅ 必须进入 RAM OS |

---

## 下载程序

从 [Releases](https://github.com/jiqing112/disk-cloner/releases) 下载对应平台：

```bash
# Linux
chmod +x disk-cloner-linux-amd64
./disk-cloner-linux-amd64

# Windows
disk-cloner-windows-amd64.exe
```

---

## 模式 1 — 克隆到本地磁盘

把远程服务器的硬盘完整复制到本地硬盘。**程序运行在目标机（接收端）上**。

### 数据流

```
源服务器 A (Alpine RAM OS)          目标服务器 B (Alpine RAM OS)
┌───────────────────┐               ┌─────────────────────┐
│  dd if=/dev/sda    │ ──SSH──▶     │  gunzip             │
│  ↓                 │               │  ↓                  │
│  gzip -1 或 pigz   │  压缩流传输    │  dd of=/dev/sda    │
└───────────────────┘               │  ↑ 程序在 RAM 中运行 │
                                      └─────────────────────┘
```

### 操作步骤

1. **两台服务器都进入 Alpine RAM OS**：
   ```bash
   # 在服务器 A 和 B 上分别执行
   bash reinstall.sh alpine --hold 1
   ```

2. **上传程序到目标机 B**：
   ```bash
   scp disk-cloner-linux-amd64 root@B_IP:/tmp/
   ```

3. **在目标机 B 上运行**：
   ```bash
   ssh root@B_IP
   cd /tmp && chmod +x disk-cloner-linux-amd64 && ./disk-cloner-linux-amd64
   ```

4. **按提示操作**：
   - 输入源服务器 A 的 SSH 信息（IP、端口、用户名、密码）
   - 程序自动检测 A 是否处于 Alpine RAM OS
   - 远程磁盘列表显示后，选择源磁盘序号
   - 选择操作模式 [1]
   - 选择本地目标磁盘序号
   - 设置块大小（默认 4M，回车确认）
   - 选择压缩级别（0-9，回车默认 1）
   - 选择压缩方式（1=gzip 单核，2=pigz 多核）
   - 选择是否零填充（Y=填充空闲空间提高压缩率，n=跳过）
   - 选择是否重建 initramfs（Y=包含所有硬件驱动，恢复到不同硬件可正常启动）
   - 输入 yes 确认开始克隆
   - 等待进度条完成

5. **克隆完成后**，程序会显示 3 次 fstab 警告，提示手动检查挂载配置

### 程序在 RAM 中运行，不会被 dd 覆盖

```
目标服务器 B 启动 Alpine RAM OS 后：
┌────────────────────────────────────┐
│  tmpfs 根文件系统 (RAM 内存)        │
│  ├── /tmp/disk-cloner-linux-amd64  │  ← 程序在内存里
│  └── 运行时临时文件                 │
├────────────────────────────────────┤
│  /dev/sda — 物理硬盘 (未挂载)       │  ← dd 写入的目标
└────────────────────────────────────┘
```

---

## 模式 2 — 保存为 gzip 文件

把远程磁盘保存为本地 `.img.gz` 压缩文件。支持备份单个磁盘或全部磁盘。

### 数据流

```
远程: dd | gzip -N → SSH → 本地: 直接写入 .img.gz 文件
```

### 操作步骤

1. **源服务器进入 Alpine RAM OS**：
   ```bash
   bash reinstall.sh alpine --hold 1
   ```

2. **运行程序**（在本地机器上）：
   ```bash
   ./disk-cloner-linux-amd64
   # 或 Windows:
   disk-cloner-windows-amd64.exe
   ```

3. **输入远程 SSH 信息**（连接到源服务器）

4. **选择操作模式 [2]**

5. **选择源磁盘**：
   - 输入 `[0]` = 备份全部磁盘（一次性设置后逐个备份）
   - 输入序号 = 备份指定磁盘

6. **选择保存目录**：
   - 回车 = 当前目录下自动创建日期子目录（如 `2026-07-03/`）
   - 输入路径 = 保存到指定目录
   - Windows 上输入 `b` = 弹出文件夹浏览对话框

7. **确认文件名**（默认格式：`IP-磁盘名-容量-日期.img.gz`）

8. **设置压缩参数**：
   - 块大小（默认 4M）
   - 压缩级别（0=不压缩，1=最快，6=均衡，9=最小，回车默认 1）
   - 压缩方式（1=gzip 单核，2=pigz 多核并行）

9. **选择是否零填充**（Y=填充空闲空间，大幅提高压缩率）

10. **选择是否重建 initramfs**（Y=包含所有硬件驱动，恢复到不同硬件可正常启动）

11. **输入 yes 确认开始**

12. **完成后**：
    - 自动生成 SHA256 校验文件 `.sha256`
    - 显示文件大小和压缩率
    - 提示继续其他操作或退出

### 输出目录结构

```
./2026-07-03/
├── 192.168.1.100-sda-30G-2026-07-03.img.gz
├── 192.168.1.100-sda-30G-2026-07-03.img.gz.sha256
├── 192.168.1.100-sdb-70G-2026-07-03.img.gz
└── 192.168.1.100-sdb-70G-2026-07-03.img.gz.sha256
```

### 压缩包兼容性

生成的 `.img.gz` 是标准 gzip 格式（RFC 1952），可以在任何 Linux 上恢复：

```bash
# 用标准 gunzip + dd 恢复
gunzip -c 192.168.1.100-sda-30G-2026-07-03.img.gz | dd of=/dev/sda bs=4M

# 或者先解压再 dd
gzip -d 192.168.1.100-sda-30G-2026-07-03.img.gz
dd if=192.168.1.100-sda-30G-2026-07-03.img of=/dev/sda bs=4M
```

---

## 模式 3 — 恢复文件到远程磁盘

把本地 `.img.gz` 备份恢复到远程硬盘。

### 数据流

```
本地: .img.gz → gunzip → SSH stdin → 远程: dd of=/dev/sda
```

### 操作步骤

1. **目标服务器进入 Alpine RAM OS**：
   ```bash
   bash reinstall.sh alpine --hold 1
   ```

2. **运行程序**（在本地机器上）：
   ```bash
   ./disk-cloner-linux-amd64
   # 或 Windows:
   disk-cloner-windows-amd64.exe
   ```

3. **输入目标服务器的 SSH 信息**

4. **选择操作模式 [3]**

5. **选择源磁盘**（用于默认目标磁盘提示）

6. **选择本地文件**：
   - 回车 = 弹出文件选择对话框（Windows）或浏览当前目录（Linux）
   - 输入路径 = 直接指定文件
   - Windows 上可拖拽文件到 cmd 窗口

7. **确认远程目标磁盘**（回车使用默认，或输入其他磁盘路径）

8. **程序自动检查**：
   - SHA256 校验文件完整性（如有 .sha256 文件）
   - 目标磁盘大小 vs 解压后镜像大小（目标过小时警告）

9. **输入 yes 确认开始恢复**

10. **完成后**提示继续其他操作或退出

---

## 命令行模式

```bash
# 克隆远程磁盘到本地
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -t /dev/sda -y

# 保存为文件（自动命名+日期目录）
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -o auto -y

# 恢复文件到远程磁盘
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -r backup.img.gz -y

# 最高压缩
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -o auto -z 9 -y

# 不压缩（局域网快速传输）
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -o auto -z 0 -y

# 使用 pigz 多核压缩
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -o auto -z 1 -y
```

### 参数表

| 参数 | 说明 | 默认 |
|------|------|------|
| `-H` | 远程服务器 IP | — |
| `-P` | SSH 端口 | 22 |
| `-u` | SSH 用户名 | root |
| `-p` | SSH 密码（不提供则尝试密钥） | — |
| `-s` | 源磁盘路径（远程） | — |
| `-t` | 目标磁盘路径（本地） | — |
| `-o` | 保存为 gzip 文件，`auto` 自动命名 | — |
| `-r` | 从 gzip 文件恢复到远程磁盘 | — |
| `-bs` | dd 块大小 | 4M |
| `-z` | 压缩级别 0-9（0=不压缩，1=最快，9=最小） | 1 |
| `-y` | 跳过确认 | false |
| `-V` | 显示版本号 | — |
| `--fix-boot-disk` | 修复引导（独立模式） | — |

---

## 关键功能详解

### 零填充

零填充在 dd 之前对远程磁盘的每个可挂载分区写入全零，把空闲空间变成连续零字节。gzip 对连续零的压缩率极高（接近 1000:1），所以零填充后：

```
100 GB 磁盘，实际数据 14 GB
  不零填充: 压缩包 ~50-90 GB（空闲空间残留数据压不动）
  零填充后: 压缩包 ~14 GB（空闲空间全零，极高压缩率）
```

支持 LVM 逻辑卷：自动 `vgscan + vgchange -ay` 激活卷组后挂载填充。零填充过程中每 5 秒显示已写入进度。

### initramfs 重建

备份前可选择重建远程系统的 initramfs，使其包含**所有硬件驱动**（virtio、nvme、SCSI 等）。这样备份的镜像恢复到不同硬件的机器上也能正常启动。

原理：`dracut --no-hostonly` 把 `/lib/modules/` 下所有内核模块都打包进 initramfs，而不是只包含当前硬件的驱动。

支持的工具（自动检测）：
- `dracut`（Fedora / RHEL / CentOS / Rocky / Alma）
- `update-initramfs`（Debian / Ubuntu / Kali）
- `mkinitcpio`（Arch / Manjaro）

也可以在进入 Alpine RAM OS 之前，在正常运行的系统上手动执行：
```bash
# Fedora / RHEL / CentOS
dracut --no-hostonly --force --regenerate-all

# Debian / Ubuntu
update-initramfs -u -k all
```

### SHA256 校验

保存时自动生成 `.sha256` 校验文件。恢复时如果存在校验文件，自动验证文件完整性。校验失败时提示是否强制恢复。

### 压缩方式

| 方式 | 说明 | 速度 | 兼容性 |
|------|------|------|--------|
| gzip | 单核压缩 | 较慢 | 所有系统自带 |
| pigz | 多核并行压缩 | 快 3-4 倍 | 需远程安装 `apk add pigz` |
| 不压缩 (-z 0) | 裸 dd 传输 | 局域网最快 | 无需压缩工具 |

所有方式产出的 `.img.gz` 都是标准 gzip 格式，完全兼容。

### 进度显示

每秒刷新实时进度，使用 30 秒滑动窗口平均速度计算稳定 ETA：

```
  [================>-----------------------]  45.2%  118.5 MB/s  18.1GB/40.0GB  用时: 2分35秒  ETA: 2分55秒
```

### 日期目录

保存时自动创建日期子目录（格式 `年-月-日`，如 `2026-07-03`），文件名包含 IP、磁盘名、容量、日期：

```
./2026-07-03/192.168.1.100-sda-30G-2026-07-03.img.gz
```

### 备份全部磁盘

模式 2 选择 `[0]` 可一次性备份远程所有磁盘，共享同一组设置（保存目录、压缩级别、零填充等），逐个磁盘执行备份。

### IP 自动提取

输入服务器 IP 时，如果复制了带其他文字的内容（如 `IP: 192.168.1.100:22`），程序自动提取其中的 IP 地址。

### 交互菜单返回

所有选择菜单输入 `q` 可返回上一步，不需要重新连接 SSH。

---

## 克隆后或许做的事(非必须)

克隆到磁盘完成后，程序会显示 3 次警告。**重启前请务必执行**：
这个主要针对fstab挂载多个磁盘的情况，比如说一个服务器有sda、sdb、sdc三个盘，如果只克隆恢复了sda，开机后，可能会因为fstab的(sdb、sdc)挂载信息导致进入开机卡慢甚至无法开机。所以需要注释掉fstab里其他盘的挂载信息。

```bash
# 1. 创建设备节点（Alpine 精简环境需要）
mdev -s

# 2. 用 lsblk 查看分区号
lsblk
# 挂载根分区（根据实际分区号调整）
mount /dev/sda4 /mnt

# 3. 编辑 fstab，注释掉源服务器独有的数据盘挂载
vi /mnt/etc/fstab
# 注释掉 /data、/mnt/* 等不存在的磁盘条目

# 4. 卸载并重启
umount /mnt
reboot
```

> 不处理 fstab → systemd 等不存在的设备 90 秒 → 进入 emergency mode

---

## 平台说明

### Linux

- 程序启动时自动 `apk add util-linux lvm2 e2fsprogs xfsprogs efibootmgr`
- 三种模式全支持
- 输入使用 terminal raw 模式，退格/Ctrl+U/Ctrl+W 完整支持
- 文件路径输入支持 Tab 补全

### Windows

- **仅支持保存 [2] 和恢复 [3]**（不克隆硬盘）
- 不需要安装 SSH 或 gzip — Go 程序已内置
- 恢复时回车弹出文件选择框，支持拖拽、手动输入
- 保存时回车弹出文件夹浏览对话框
- 程序自动设置 UTF-8 编码，中文路径正常
- 最小化到任务栏不影响传输（异步刷新）

---

## 技术架构

| 组件 | 实现 |
|------|------|
| SSH 协议 | Go `crypto/ssh`（纯 Go，不依赖系统 ssh） |
| gzip 压缩 | 远程 busybox gzip / pigz |
| gzip 解压 | Go `compress/gzip`（本地） |
| dd 读写 | 远程 busybox dd |
| 磁盘扫描 | 远程 `lsblk` |
| 文件对话框 | PowerShell WinForms（Windows） |
| 文件校验 | SHA256 |
| initramfs 重建 | chroot + dracut / update-initramfs / mkinitcpio |
| LVM 支持 | vgscan + vgchange -ay |
| 进度刷新 | 异步 Sync，窗口最小化不影响 |

---

## 自行编译

```bash
git clone https://github.com/jiqing112/disk-cloner
cd disk-cloner

# Linux
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o disk-cloner-linux-amd64 .

# Windows
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o disk-cloner-windows-amd64.exe .
```

Windows 下也可运行 `build.bat`。

## 自动发布 + 快速提交

推送版本标签触发 GitHub Actions 自动编译：

```bash
git tag v1.0.0 && git push origin v1.0.0
```

双击 `push.bat` 快速提交代码（自动 add/commit/push，支持打 tag）。

---

## License

WTFPL & Beerware
