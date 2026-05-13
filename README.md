# Disk Cloner

通过 SSH 将远程服务器的硬盘克隆到本地，或在本地保存为 gzip 压缩镜像文件。

专为 **Alpine Linux RAM OS** 环境设计——客户端和远程服务器都可以是 Alpine Linux 的 RAM 启动环境，无需安装系统。克隆时自动使用 gzip 压缩传输，大幅减少网络流量。

## 适用场景

- **云服务器迁移**：将云服务器硬盘 dd 到本地，再恢复到其他机器
- **系统备份**：远程磁盘 → 本地 gzip 压缩文件
- **救援模式克隆**：源服务器进入 Alpine RAM OS 后，通过网络克隆其磁盘

## 前置条件

- 客户端和远程服务器均能通过 SSH 连接（密码或密钥）
- 远程服务器需要 `dd`（Alpine busybox 自带）
- 客户端会自动安装所需依赖（`apk add`）

## 快速开始

### 构建

```bash
# Windows 下构建
build.bat

# Linux/Mac 下直接构建
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o disk-cloner-linux-amd64 .
```

### 交互模式（推荐）

将程序放到客户端 Alpine Linux 上，直接运行：

```bash
./disk-cloner-linux-amd64
```

按照提示输入：
1. 远程服务器 IP
2. SSH 端口（默认 22）
3. 用户名（默认 root）
4. 密码（回车则使用 SSH 密钥）

然后选择源盘和操作模式：

```
  操作模式:
  [1] 克隆到本地磁盘 (dd → 磁盘)
  [2] 保存为压缩文件 (dd → gzip 文件)
```

### 命令行模式

```bash
# 克隆磁盘
./disk-cloner-linux-amd64 -H 192.168.1.100 -p password \
  -s /dev/sda -t /dev/sda -y

# 保存为压缩文件（自动命名：IP-磁盘名.img.gz）
./disk-cloner-linux-amd64 -H 192.168.1.100 -p password \
  -s /dev/sda -o auto -y

# 自定义文件名
./disk-cloner-linux-amd64 -H 192.168.1.100 -p password \
  -s /dev/sda -o backup.img.gz -y

# 使用 SSH 密钥（不提供 -p 参数）
./disk-cloner-linux-amd64 -H 192.168.1.100 -s /dev/sda -t /dev/nvme0n1 -y
```

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-H` | 远程服务器 IP | — |
| `-P` | SSH 端口 | 22 |
| `-u` | SSH 用户名 | root |
| `-p` | SSH 密码（不提供则使用密钥） | — |
| `-s` | 源磁盘（远程），如 `/dev/sda` | — |
| `-t` | 目标磁盘（本地），如 `/dev/sda` | — |
| `-o` | 保存为文件，`auto` 为自动命名 | — |
| `-bs` | dd 块大小 | 4M |
| `-y` | 跳过确认 | — |
| `--fix-boot-disk` | 离线修复引导（独立模式） | — |

## 克隆后必须做的事

克隆完成后程序会连续提示 3 次——**务必在重启前执行**：

```bash
# 1. 创建设备节点（Alpine 精简环境需要）
mdev -s

# 2. 挂载根分区（用 lsblk 查看分区号）
mount /dev/sda4 /mnt

# 3. 编辑 fstab，删除云服务器独有的数据盘挂载
vi /mnt/etc/fstab

# 注释掉类似这样的行（在新机器上不存在）：
# UUID=xxx  /data       ext4  defaults  0 2
# /dev/vdb  /mnt/logs   ext4  defaults  0 0

# 4. 卸载并重启
umount /mnt
reboot
```

> 如果不修改 fstab，systemd 会在启动时等待不存在的设备 90 秒，可能导致系统进入 emergency mode。

## 修复引导（可选）

如果克隆后系统能启动但仍想修复 initramfs 和 GRUB：

```bash
# 确保安装了必要的包
apk add util-linux lvm2 e2fsprogs xfsprogs btrfs-progs efibootmgr

# 对目标磁盘执行引导修复
./disk-cloner-linux-amd64 --fix-boot-disk /dev/sda
```

此操作会：
- 重建 initramfs（包含所有硬件驱动，`--no-hostonly`）
- 重装 GRUB 引导器
- 自动修复 fstab（注释掉数据盘条目，其余加 `nofail`）

## 工作原理

```
远程服务器                          本地客户端
┌──────────────┐                   ┌──────────────┐
│  dd          │                   │  写入目标磁盘  │
│  ↓           │                   │  ↑           │
│  gzip -1     │ ═══ SSH 管道 ═══ │  gunzip      │
│  (压缩传输)   │   (压缩后数据)    │  (本地解压)   │
└──────────────┘                   └──────────────┘
```

1. 远程执行 `dd if=/dev/sda | gzip -1`，磁盘数据压缩后通过 SSH 传输
2. 本地接收压缩流，解压后写入目标设备
3. 传输前自动对远程磁盘执行零填充（将空闲空间写零），提高压缩率
4. 进度实时显示：百分比、速度、预计剩余时间

## 零填充

克隆和保存文件前都会自动对远程磁盘的空闲空间执行零填充：

- 临时挂载每个分区 → `dd if=/dev/zero` 填满剩余空间 → 删除零文件 → 卸载
- 空闲空间变为全零 → gzip 压缩率极高（接近 100%）
- 40GB 源盘、6GB 实际数据 → 网络只需传输约 6GB

零填充可能因远程缺少文件系统模块而跳过（不影响克隆）。

## 注意事项

- Alpine Linux 使用 `mdev` 而非 `udev`。挂载分区前可能需要 `mdev -s` 创建设备节点
- 源盘和目标盘可以不同大小。目标较小会警告但允许继续（超过目标容量的数据会丢失）
- 保存文件模式将文件写入当前工作目录，RAM OS 下注意内存充足
- SSH 密钥认证优先于密码认证。密码通过内存传输，不写入磁盘
