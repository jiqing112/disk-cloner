# Disk Cloner

通过 SSH 远程克隆磁盘的 Go 工具。纯 Go 实现，单文件零依赖。支持 Linux 和 Windows。

## 功能一览

| 模式 | 方向 | 用途 |
|------|------|------|
| 克隆 | 远程 → 本地磁盘 | 服务器迁移、对拷 |
| 保存 | 远程 → 本地文件（单盘或全部） | 系统备份、镜像存档 |
| 恢复 | 本地文件 → 远程磁盘 | 系统还原、批量部署 |

---

## 三种模式详解

### 模式 1 — 克隆到本地磁盘

```
源服务器 A (Alpine RAM OS)           目标服务器 B (Alpine RAM OS)
┌──────────────┐                     ┌──────────────────────┐
│  dd if=/dev/sda │ ──SSH──▶        │  gunzip              │
│  ↓              │                  │  ↓                   │
│  gzip -N       │  压缩流传输       │  dd of=/dev/sda     │
└──────────────┘                     └──────────────────────┘
```

程序在目标机 B 的 RAM 中运行，`dd of=/dev/sda` 写入物理硬盘，两者互不干扰。

**操作步骤**：
1. 两台服务器都用 `bash reinstall.sh alpine --hold 1` 进入 Alpine RAM OS
2. 在目标机 B 上上传程序并运行：
   ```bash
   scp disk-cloner-linux-amd64 root@B_IP:/tmp/
   ssh root@B_IP
   cd /tmp && chmod +x disk-cloner-linux-amd64 && ./disk-cloner-linux-amd64
   ```
3. 程序通过 SSH 连接源机 A，读取 A 的硬盘，写入 B 的硬盘

### 模式 2 — 保存为 gzip 文件

把远程磁盘保存为本地 `.img.gz` 压缩文件。支持**备份全部磁盘**（选 `[0]`）。

```
远程: dd | gzip -N → SSH → 本地: 写入 .img.gz
```

- 自动创建日期目录 `2026-06-15/`，文件名包含日期和容量
- 生成 SHA256 校验文件 `.sha256`
- 可配置压缩级别 `-z 0`(不压缩) / `-z 1`(最快) / `-z 9`(最小)

### 模式 3 — 恢复文件到远程磁盘

把本地 `.img.gz` 备份恢复到远程硬盘。

```
本地: .img.gz → gunzip → SSH stdin → 远程: dd of=/dev/sda
```

- 恢复前自动 SHA256 校验
- Windows 支持文件对话框、拖拽、目录浏览三种选文件方式
- 恢复前校验文件完整性

---

## 参数表

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

## 示例

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
```

---

## 进度显示

所有模式都有每秒刷新的实时进度，含已用时间和 ETA：

```
  [================>-----------------------]  45.2%  118.5 MB/s  18.1GB/40.0GB  用时: 2分35秒  ETA: 2分55秒
```

---

## 输出目录结构

```
./2026-06-15/
├── 192.168.1.100-vda-30G-2026-06-15.img.gz
├── 192.168.1.100-vda-30G-2026-06-15.img.gz.sha256
├── 192.168.1.100-vdb-70G-2026-06-15.img.gz
└── 192.168.1.100-vdb-70G-2026-06-15.img.gz.sha256
```

---

## 交互式流程

```
  远程服务器配置
  ─────────────────────────────────────────────
  服务器IP: 192.168.1.100
  SSH 端口 [22]:
  用户名 [root]:
  密码: ********

  正在连接...
  SSH 连接成功 (root@192.168.1.100:22)

  远程状态: Alpine Linux RAM OS (tmpfs 根文件系统)
  磁盘分区未挂载，可以安全克隆。

  发现 2 块远程磁盘

===============================================
  远程磁盘 (192.168.1.100)
===============================================
  [1] /dev/vda            30 GB
  [2] /dev/vdb            70 GB

  操作模式 — 输入序号选择:
  [1] 克隆到本地磁盘
  [2] 保存为压缩文件
  [3] 恢复文件到远程磁盘
  请输入序号: 2

===============================================
  选择源磁盘 — 输入序号选定远程磁盘
===============================================
  [1] /dev/vda            30 GB
  [2] /dev/vdb            70 GB
  [0] 备份全部磁盘
  请输入序号: 1

  当前目录: /home/user
  保存目录 (回车使用当前目录，或输入路径) [.]:
  保存目录: /home/user/2026-06-15

  文件名 [/home/user/2026-06-15/192.168.1.100-vda-30G-2026-06-15.img.gz]:

  压缩级别:
    0 = 不压缩 (局域网快速, 节省远程 CPU)
    1 = 最快 (默认, 适合日常备份)
    6 = 均衡 (中等压缩率)
    9 = 最小 (最高压缩, 费 CPU)
  压缩级别 [1]:

  零填充 (回车=执行填充, 输入n=跳过填充) [Y/n]:
```

---

## 平台说明

### Linux

- 程序启动时自动 `apk add util-linux lvm2 e2fsprogs xfsprogs efibootmgr`
- 三种模式全支持
- 输入使用 terminal raw 模式，退格/Ctrl+U/Ctrl+W 完整支持

### Windows

- **仅支持保存 [2] 和恢复 [3]**
- 不需要安装 SSH 或 gzip — Go 程序已内置
- 恢复时回车弹出文件选择框，支持拖拽
- 程序自动设置 UTF-8 编码，中文路径正常

---

## 技术架构

| 组件 | 实现 |
|------|------|
| SSH 协议 | Go `crypto/ssh`（纯 Go，不依赖系统 ssh） |
| gzip 压缩 | 远程 busybox gzip（保存）/ 本地 Go gzip（恢复） |
| dd 读写 | 远程 busybox dd |
| 磁盘扫描 | 远程 `lsblk` |
| 文件对话框 | PowerShell WinForms（Windows） |
| 文件校验 | SHA256 |

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
