# Disk Cloner

通过 SSH 远程克隆磁盘的 Go 工具。支持三个方向：

```
克隆:  远程 dd | gzip → SSH → 本地 gunzip → 磁盘
保存:  远程 dd | gzip → SSH → 本地文件 (.img.gz)
恢复:  本地 .img.gz → gunzip → SSH → 远程 dd
```

纯 Go 实现，单个可执行文件，零依赖。同时提供 Linux 和 Windows 版本。

---

## 适用场景

- **云服务器迁移**：把 A 服务器的系统盘 dd 克隆到 B 服务器(两个服务器都要进入Alpine RAM OS状态)  
- **系统备份**：远程磁盘保存为本地 gzip 压缩镜像(只需要被克隆服务器进入Alpine RAM OS状态)  
- **系统还原**：把本地备份镜像恢复到远程磁盘(只需要被克隆服务器进入Alpine RAM OS状态)  
- **救援模式克隆**：源端和目标端都进入 Alpine Linux RAM OS，安全离线克隆

---

## 快速开始

### 1. 源服务器进入 Alpine RAM OS

参考 [bin456789/reinstall](https://github.com/bin456789/reinstall) 脚本：

```bash
curl -O https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh
bash reinstall.sh alpine --hold 1
```

服务器重启后进入 Alpine Linux RAM OS 状态，此时系统盘分区已卸载，可以安全克隆。

### 2. 下载程序

从 [Releases]([https://github.com/jiqing112/disk-cloner-go/releases](https://github.com/jiqing112/disk-cloner/releases)) 下载对应平台的可执行文件：

- `disk-cloner-linux-amd64` — Linux (Alpine)
- `disk-cloner-windows-amd64.exe` — Windows

```bash
# Linux 客户端
chmod +x disk-cloner-linux-amd64
./disk-cloner-linux-amd64
```

```cmd
# Windows 客户端
disk-cloner-windows-amd64.exe
```

### 3. 按提示操作

输入远程 SSH 信息后，选择操作模式：

```
  操作模式:
  [1] 克隆到本地磁盘 (dd -> 磁盘)
  [2] 保存为压缩文件 (dd -> gzip 文件)
  [3] 恢复文件到远程磁盘 (gzip 文件 -> dd 远程磁盘)
```

---

## 三种模式详解

### 模式 1 — 克隆磁盘

把远程服务器的系统盘完整复制到本地磁盘。用于**云服务器迁移**或**对拷**。

```
远程: dd if=/dev/sda | gzip -1
         ↓ 压缩数据通过 SSH 传输
本地: gunzip → dd of=/dev/sda
```

- 传输前可选**零填充**：把远程磁盘的空闲空间写零，大幅提高压缩率
  - 40GB 源盘、6GB 实际数据 → 网络只需传约 6GB
- 克隆完成后提示 3 次 fstab 检查警告

### 模式 2 — 保存镜像

把远程磁盘保存为本地 gzip 压缩文件。用于**系统备份**。

```
远程: dd if=/dev/sda | gzip -1
         ↓
本地: 直接写 .img.gz 文件
```

- 文件名默认格式：`IP地址-磁盘名.img.gz`（如 `192.168.1.100-sda.img.gz`）
- 远程压缩，网络只传输压缩后的数据

### 模式 3 — 恢复镜像

把本地备份文件恢复到远程磁盘。用于**系统还原**。

```
本地: 读 .img.gz → gunzip
         ↓ 通过 SSH stdin 传输
远程: dd of=/dev/sda
```

- 支持恢复之前通过模式 2 保存的 `.img.gz` 文件
- 直接覆盖远程磁盘，操作前会确认

---

## 命令行模式

```bash
# 克隆磁盘
./disk-cloner-linux-amd64 -H 192.168.1.100 -p password \
  -s /dev/sda -t /dev/sda -y

# 保存为文件（自动命名）
./disk-cloner-linux-amd64 -H 192.168.1.100 -p password \
  -s /dev/sda -o auto -y

# 保存为指定文件
./disk-cloner-linux-amd64 -H 192.168.1.100 -p password \
  -s /dev/sda -o backup.img.gz -y

# 恢复文件到远程磁盘
./disk-cloner-linux-amd64 -H 192.168.1.100 -p password \
  -s /dev/sda -r backup.img.gz -y

# 使用 SSH 密钥（不提供 -p）
./disk-cloner-linux-amd64 -H 192.168.1.100 -s /dev/sda -t /dev/nvme0n1 -y
```

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-H` | 远程服务器 IP | — |
| `-P` | SSH 端口 | 22 |
| `-u` | SSH 用户名 | root |
| `-p` | SSH 密码（不提供则尝试密钥） | — |
| `-s` | 源磁盘（远程），如 `/dev/sda` | — |
| `-t` | 目标磁盘（本地），如 `/dev/sda` | — |
| `-o` | 保存为文件，`auto` 为自动命名 | — |
| `-r` | 从 gzip 文件恢复到远程 | — |
| `-bs` | dd 块大小 | 4M |
| `-y` | 跳过确认 | — |

---

## 克隆后必须做的事

克隆完成后程序会连续提示 3 次——**务必在重启前执行**：

```bash
# 1. 创建设备节点（Alpine 精简环境需要）
mdev -s

# 2. 挂载根分区（用 lsblk 查看分区号）
mount /dev/sda4 /mnt

# 3. 编辑 fstab，删除源服务器独有的数据盘挂载
vi /mnt/etc/fstab
# 注释掉 /data、/mnt/* 等不存在的磁盘条目

# 4. 卸载并重启
umount /mnt && reboot
```

> 不修改 fstab 的话，systemd 会等不存在的设备 90 秒，可能进入 emergency mode。

---

## Windows 版说明

- **仅支持模式 2（保存）和模式 3（恢复）**
- **不需要安装 SSH 客户端** — Go 程序内置 SSH 协议实现
- **不需要 gzip** — Go 程序内置压缩/解压
- 双击运行或命令行执行，操作流程与 Linux 版相同

---

## 自行编译

```bash
# 需要 Go 1.22+
git clone https://github.com/xxx/disk-cloner-go
cd disk-cloner-go

# Linux
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o disk-cloner-linux-amd64 .

# Windows
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o disk-cloner-windows-amd64.exe .
```

Windows 下也可直接运行 `build.bat`。

---

## 前置依赖

### 客户端（程序运行的地方）

- **Linux**：程序启动时自动 `apk add util-linux lvm2 e2fsprogs xfsprogs efibootmgr`
- **Windows**：无需任何依赖

### 远程服务器（被克隆的机器）

- 需要 `dd`（Alpine busybox 自带）
- 需要 `gzip`（Alpine busybox 自带，程序会自动检测并安装）
- 建议进入 Alpine Linux RAM OS 后再克隆（程序会自动检测并警告）

---

## 工作原理

| 组件 | 运行位置 | 实现 |
|------|---------|------|
| SSH 客户端 | 本地 | Go `crypto/ssh` |
| gzip 压缩/解压 | 两端 | Go `compress/gzip`（本地）/ 远程 busybox gzip |
| dd 读写磁盘 | 远程 | 通过 SSH 执行 dd 命令 |
| lsblk 磁盘扫描 | 远程/本地 | 通过 SSH 执行 / 本地执行 |
| mdev/partprobe | 本地 | 通过 `--fix-boot-disk` 离线修复引导 |
