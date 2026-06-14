# Disk Cloner

通过 SSH 远程克隆磁盘的 Go 工具。纯 Go 实现，单文件零依赖。

## 功能一览

| 模式 | 方向 | 用途 |
|------|------|------|
| 克隆 | 远程 → 本地磁盘 | 服务器迁移、对拷 |
| 保存 | 远程 → 本地文件 | 系统备份、镜像存档 |
| 恢复 | 本地文件 → 远程磁盘 | 系统还原、批量部署 |

支持 Linux 和 Windows。

---

## 三种模式详解

### 模式 1 — 克隆到本地磁盘

把远程服务器的硬盘完整复制到本地硬盘。**程序运行在目标机（接收端）上**。

```
源服务器 A (Alpine RAM OS)           目标服务器 B (Alpine RAM OS)
┌──────────────┐                     ┌──────────────────────┐
│  dd if=/dev/sda │ ──SSH──▶        │  gunzip              │
│  ↓              │                  │  ↓                   │
│  gzip -1       │  压缩流传输       │  dd of=/dev/sda     │
└──────────────┘                     │  ↑ RAM 中运行程序    │
                                     │  ↑ 二进制也在 RAM 中  │
                                     └──────────────────────┘
```

**关键点 — RAM OS 不会覆盖程序本身**：

```
目标服务器 B 启动 Alpine RAM OS 后的内存布局:
┌────────────────────────────────────┐
│  tmpfs 根文件系统 (RAM)             │
│  ├── /usr/bin/disk-cloner    ← 程序│  ← scp 传进来存内存里
│  └── 运行时的临时文件                │
├────────────────────────────────────┤
│  /dev/sda — 物理硬盘 (未挂载)      │  ← dd 写入的目标
└────────────────────────────────────┘
```

程序在 RAM 里运行，`dd of=/dev/sda` 写入物理硬盘，两者互不干扰。重启后 RAM 清空（程序消失），物理硬盘上的克隆数据保留。

**操作步骤**：
1. 两台服务器都用 `bash reinstall.sh alpine --hold 1` 进入 Alpine RAM OS
2. 在**目标机 B** 上，通过 SCP 上传程序（存在 tmpfs RAM 中）：
   ```bash
   scp disk-cloner-linux-amd64 root@B_IP:/tmp/
   ```
3. 在目标机 B 上运行：
   ```bash
   cd /tmp
   chmod +x disk-cloner-linux-amd64
   ./disk-cloner-linux-amd64
   ```
4. 程序通过 SSH 连接源机 A，读取 A 的硬盘，写入 B 的硬盘

### 模式 2 — 保存为 gzip 文件

### 模式 2 — 保存为 gzip 文件

把远程磁盘保存为本地 `.img.gz` 压缩文件。

```
远程服务器                         本地客户端
┌──────────────┐                  ┌──────────────┐
│  dd if=/dev/sda │ ──SSH──▶     │  直接写文件    │
│  ↓              │               │  xxx.img.gz  │
│  gzip -1       │               │              │
└──────────────┘                  └──────────────┘
```

- 远程压缩后传输，带宽占用最小
- 零填充同样可用
- 文件名默认为 `IP-磁盘名.img.gz`

### 模式 3 — 恢复文件到远程磁盘

把本地 `.img.gz` 备份恢复到远程硬盘。模式 2 的逆操作。

```
本地客户端                         远程服务器
┌──────────────┐                  ┌──────────────┐
│  xxx.img.gz  │                  │  dd of=/dev/sda│
│  ↓ (gunzip)  │ ──SSH stdin──▶ │              │
└──────────────┘                  └──────────────┘
```

- 本地解压后通过 SSH 管道传入远程 dd
- Windows 支持文件对话框 + 拖拽 + 目录浏览三种选文件方式
- 恢复前**目标远程服务器需进入 Alpine RAM OS**，确保磁盘分区已卸载
- 进度显示解压后字节数（从 gzip 尾部 ISIZE 读取，≤4GB 精确）

---

## 使用方式

### 1. 服务器进入 Alpine RAM OS

被克隆的源服务器**必须**进入 Alpine RAM OS。克隆到本地磁盘（模式 1）时，目标服务器也需进入 Alpine RAM OS。保存文件（模式 2）和恢复文件（模式 3）只需源端或目标端进入。

参考 [bin456789/reinstall](https://github.com/bin456789/reinstall)：

```bash
bash reinstall.sh alpine --hold 1
```

重启后进入 Alpine RAM OS。此时物理磁盘分区已卸载，可安全克隆。

### 2. 下载程序

从 [Releases](https://github.com/jiqing112/disk-cloner/releases) 下载：

```bash
# Linux
chmod +x disk-cloner-linux-amd64
./disk-cloner-linux-amd64

# Windows
disk-cloner-windows-amd64.exe
```

### 3. 交互式操作

按提示输入远程 SSH 信息、选择源盘、选择操作模式即可。

```
  远程服务器配置
  ─────────────────────────────────────────────
  服务器IP: 192.168.1.100
  SSH 端口 [22]:
  用户名 [root]:
  密码 (回车使用密钥):

  正在检测 SSH 服务...
  SSH 服务已确认 (SSH-2.0-OpenSSH_9.6)
  正在进行 SSH 认证...
  SSH 连接成功 (root@192.168.1.100:22)
```

每个大步骤之间有 `───` 分隔，一目了然。

### 4. 命令行模式

```bash
# 克隆远程磁盘到本地
./disk-cloner-linux-amd64 -H 192.168.1.100 -p password \
  -s /dev/sda -t /dev/sda -y

# 保存为文件（自动命名）
./disk-cloner-linux-amd64 -H 192.168.1.100 -p password \
  -s /dev/sda -o auto -y

# 恢复文件到远程磁盘
./disk-cloner-linux-amd64 -H 192.168.1.100 -p password \
  -s /dev/sda -r backup.img.gz -y
```

#### 参数表

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-H` | 远程服务器 IP | — |
| `-P` | SSH 端口 | 22 |
| `-u` | SSH 用户名 | root |
| `-p` | SSH 密码（不提供则尝试密钥） | — |
| `-s` | 源磁盘路径（远程） | — |
| `-t` | 目标磁盘路径（本地） | — |
| `-o` | 保存为 gzip 文件，`auto` 自动命名 | — |
| `-r` | 从 gzip 文件恢复到远程磁盘 | — |
| `-bs` | dd 块大小 | 4M |
| `-y` | 跳过确认提示 | false |
| `--fix-boot-disk` | 离线修复引导（实验性） | — |

---

## 克隆后必须做的事

克隆到磁盘完成后，程序会连续弹出 3 次警告。**重启前请务必执行**：

```bash
# 1. 创建设备节点（Alpine 精简环境需要）
mdev -s

# 2. 挂载根分区（用 lsblk 查看分区号）
lsblk
mount /dev/sda4 /mnt

# 3. 编辑 fstab，注释掉源服务器独有的数据盘挂载
vi /mnt/etc/fstab
# 注释掉 /data、/mnt/* 等不存在的磁盘条目

# 4. 卸载并重启
umount /mnt
reboot
```

> 不处理 fstab → systemd 等不存在的设备 90 秒 → 进入 emergency mode。

---

## 平台说明

### Linux

- 程序启动时自动 `apk add util-linux lvm2 e2fsprogs xfsprogs efibootmgr`
- 克隆到磁盘、保存本地文件、恢复文件三模式全支持
- 输入使用 terminal raw 模式，退格/Ctrl+U/Ctrl+W 完整支持

### Windows

- **仅支持保存文件 [2] 和恢复文件 [3]**（不克隆硬盘）
- 不需要安装 SSH 客户端或 gzip — Go 程序已内置
- 恢复时文件选择三种方式：
  - **回车** → 弹出 Windows 原生文件选择对话框
  - **拖拽** → 把文件拖到 cmd 窗口自动填入路径
  - **手动输入**路径
- 程序启动时自动 `chcp 65001` 切换到 UTF-8 模式，中文路径正常
- 进度条每 1 秒 `os.Stdout.Sync()` 强制刷新，Windows cmd 下可见

---

## 进度显示

| 模式 | 进度条 | 百分比 | 速度 | 已传量 | ETA |
|------|--------|--------|------|--------|-----|
| 克隆（已知总量） | `[====>-----]` | 52.3% | 118.5 MB/s | 20.1GB/40.0GB | 2分48秒 |
| 保存（已知总量） | `[====>-----]` | 52.3% | 118.5 MB/s | 20.1GB/40.0GB | 2分48秒 |
| 恢复 ≤4GB（ISIZE） | `[====>-----]` | 52.3% | 118.5 MB/s | 10.5GB/20.0GB | 2分48秒 |
| 恢复 >4GB（ISIZE溢出） | 旋转动画 `\|/-\` | — | 118.5 MB/s | 10.5GB | — |

---

## 技术架构

| 组件 | 实现 | 运行位置 |
|------|------|---------|
| SSH 协议 | Go `crypto/ssh`（纯 Go） | 客户端 |
| gzip 压缩/解压 | Go `compress/gzip` + 远程 busybox gzip | 两端 |
| dd 读写磁盘 | 远程 busybox dd | 远程服务器 |
| 磁盘扫描 | 远程 `lsblk`（util-linux） | 远程服务器 |
| 控制台输入 | Linux: term raw mode 逐字符 / Windows: bufio.Reader | 客户端 |
| 文件对话框 | PowerShell 调用 WinForms OpenFileDialog | Windows 客户端 |
| 控制台编码 | `chcp 65001`（UTF-8 模式） | Windows 客户端 |
| 进度刷线 | `os.Stdout.Sync()` 每秒强制刷新 | 客户端 |

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

Windows 下也可双击 `build.bat`。

## 自动发布

推送版本标签即可触发 GitHub Actions 自动编译并上传 Release：

```bash
git tag v1.0.0
git push origin v1.0.0
```

## 快速提交

修改代码后双击 `push.bat`，输入 commit message 即可推送。

---

## License

MIT
