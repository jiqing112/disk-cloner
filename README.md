# Disk Cloner

通过 SSH 远程克隆、备份、恢复磁盘，并支持本机硬盘直传远程存储的 Go 工具。纯 Go 实现，单个可执行文件即拷即用，支持 Linux 和 Windows。

---

## 目录

- [功能一览](#功能一览)
- [前置准备：进入 Alpine RAM OS](#前置准备进入-alpine-ram-os)
- [下载程序](#下载程序)
- [模式 1 — 克隆到本地磁盘](#模式-1--克隆到本地磁盘)
- [模式 2 — 保存为 gzip 文件](#模式-2--保存为-gzip-文件)
- [模式 3 — 恢复文件到远程磁盘](#模式-3--恢复文件到远程磁盘)
- [模式 4 — 传输到远程存储](#模式-4--传输到远程存储)
- [命令行模式](#命令行模式)
- [引导修复 (GRUB + initramfs)](#引导修复-grub--initramfs)
- [关键功能详解](#关键功能详解)
- [克隆后必须做的事](#克隆后必须做的事)
- [平台说明](#平台说明)
- [技术架构](#技术架构)
- [自行编译](#自行编译)
- [自动发布](#自动发布)
- [常见问题](#常见问题)

---

## 功能一览

| 模式 | 方向 | 用途 |
|------|------|------|
| 克隆 | 远程磁盘 → 本地磁盘 | 服务器迁移、对拷 |
| 保存 | 远程磁盘 → 本地 .img.gz 文件（单盘或全部） | 系统备份、镜像存档 |
| 恢复 | 本地 .img.gz 文件 → 远程磁盘 | 系统还原、批量部署 |
| 传输 | 磁盘 → SFTP / FTP / WebDAV / S3 / Pixeldrain | 异地备份，镜像不占本地空间 |

> 传输模式（模式 4）**直接运行在被克隆的服务器上**（Alpine RAM OS），读本机硬盘推送到存储服务，连 SSH 都不需要，镜像不占任何本地磁盘空间。

---

## 前置准备：进入 Alpine RAM OS

### 为什么需要 Alpine RAM OS？

dd 操作的是**整个物理硬盘**（包括分区表、引导扇区、所有分区数据）。如果系统正在运行，文件系统已挂载，dd 读写同时进行会导致数据不一致。

Alpine RAM OS 把整个系统运行在内存（tmpfs）里，物理磁盘完全未挂载，可以安全地读写整盘。程序本身也跑在内存里，dd 写物理磁盘不会覆盖程序。

### 操作步骤

在需要操作的服务器上，使用 [bin456789/reinstall](https://github.com/bin456789/reinstall#%E5%8A%9F%E8%83%BD-3-%E9%87%8D%E5%90%AF%E5%88%B0--alpine-live-os%E5%86%85%E5%AD%98%E7%B3%BB%E7%BB%9F) 脚本进入 Alpine RAM OS：

```bash
# 国外服务器
curl -O https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh

# 国内服务器
curl -O https://cnb.cool/bin456789/reinstall/-/git/raw/main/reinstall.sh

# 执行进入 Alpine RAM OS
bash reinstall.sh alpine --hold 1
```

执行后服务器会自动重启进入 Alpine Linux 内存系统。重启完成后 SSH 登录：

- 端口：22
- 用户：root
- 密码：脚本执行时终端会显示（通常格式为 `[!] root password: xxxxx`）

### 各模式需要哪些服务器进入 RAM OS？

| 模式 | 源服务器（被克隆/备份的） | 目标服务器（接收端） |
|------|---------------------------|---------------------|
| 模式 1 克隆 | ✅ 必须进入 RAM OS | ✅ 必须进入 RAM OS |
| 模式 2 保存 | ✅ 必须进入 RAM OS | 不需要（本地接收文件） |
| 模式 3 恢复 | 不需要（本地发送文件） | ✅ 必须进入 RAM OS |
| 模式 4 传输 | ✅ 必须进入 RAM OS | 不需要（存储服务器正常在线即可） |

> 模式 4 的"本机模式"下（`-l` 参数，或交互模式选 `[4]`），程序就运行在源服务器上，连 SSH 连接都不需要，只要存储服务可达。

> 如果使用 Windows 运行程序做模式 2 或模式 3，Windows 端不需要进入 RAM OS（Windows 不做 dd 操作）。

> 接收端（克隆的目标机、恢复的目标机）硬盘容量必须 ≥ 源盘容量。整盘 dd 不会自动调整分区，目标盘偏小时会截断 ext4 元数据，导致恢复后出现 `bad block bitmap checksum` / `Journal has aborted` 错误。

---

## 下载程序

从 [Releases](https://github.com/jiqing112/disk-cloner/releases) 下载对应平台的可执行文件：

```bash
# Linux
chmod +x disk-cloner-linux-amd64
./disk-cloner-linux-amd64

# Windows
disk-cloner-windows-amd64.exe
```

---

## 模式 1 — 克隆到本地磁盘

把远程服务器 A 的硬盘完整复制到本地服务器 B 的硬盘。**程序运行在目标机 B 上**。

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

### 详细操作步骤

**第一步：两台服务器都进入 Alpine RAM OS**

在服务器 A 和 B 上分别执行：

```bash
bash reinstall.sh alpine --hold 1
```

等待两台服务器都重启完成，SSH 可连接。

**第二步：上传程序到目标机 B**

```bash
scp disk-cloner-linux-amd64 root@B_IP:/tmp/
```

**第三步：在目标机 B 上运行程序**

```bash
ssh root@B_IP
cd /tmp
chmod +x disk-cloner-linux-amd64
./disk-cloner-linux-amd64
```

**第四步：选择操作模式 [1]**

程序启动后先选择操作模式（再输入 SSH 信息）：

```
+==============================================+
|       Disk Cloner - 磁盘克隆工具 v3           |
|    远程 -> 本地 dd 克隆 (Alpine Linux)        |
+==============================================+

  操作模式 — 输入序号选择:
  [1] 克隆到本地磁盘 (dd -> 磁盘)
  [2] 保存为压缩文件 (dd -> gzip 文件)
  [3] 恢复文件到远程磁盘 (gzip 文件 -> dd 远程磁盘)
  [4] 读取本地硬盘传输到远程存储 (dd -> SFTP/FTP/WebDAV/S3)
  请输入序号: 1
```

> Windows 上只显示 [2] 和 [3]（克隆和本机传输需要 Linux 块设备）。模式 [4] 在本机（源服务器）上运行，详见 [模式 4](#模式-4--传输到远程存储)。

**第五步：输入源服务器 A 的 SSH 信息**

```
  远程服务器配置
  ─────────────────────────────────────────────
  服务器IP: 192.168.1.100
  SSH 端口 [22]:
  用户名 [root]:
  密码 (回车使用密钥):
```

- **服务器IP**：输入源服务器 A 的 IP（如果复制了带其他文字的内容如 `IP: 192.168.1.100:22`，程序会自动提取其中的 IP）
- **SSH 端口**：回车使用默认 22，或输入实际端口
- **用户名**：回车使用默认 root，或输入其他用户
- **密码**：输入密码（输入时不显示），或回车使用 SSH 密钥认证

**第六步：等待连接和扫描**

程序自动连接 SSH，检测远程是否处于 Alpine RAM OS，安装远程依赖（util-linux、gzip 等，pigz 仅在选用多核压缩时按需安装），扫描远程磁盘：

```
  正在连接...
  SSH 连接成功 (root@192.168.1.100:22)

  ─────────────────────────────────────────────

  远程状态: Alpine Linux RAM OS (tmpfs 根文件系统)
  磁盘分区未挂载，可以安全克隆。

  ─────────────────────────────────────────────
  发现 2 块远程磁盘
```

**第七步：选择源磁盘**

```
===============================================
  选择源磁盘 — 输入序号选定远程磁盘
===============================================
  [1] /dev/sda            30 GB
  [2] /dev/sdb            70 GB

  请输入序号: 1
```

> 输入 `q` 可返回上一步重新选择。

**第八步：选择本地目标磁盘**

程序扫描本地磁盘并列出：

```
===============================================
  选择目标磁盘 — 输入序号选定本地磁盘
===============================================
  [1] /dev/sda            120 GB
  [2] /dev/nvme0n1       500 GB
  请输入序号: 1
```

**第九步：设置参数**

```
  ┌────────────────────────────────────────────┐
  │  源:   192.168.1.100:/dev/sda (30 GB)     │
  │  目标: 本地 /dev/sda (120 GB)             │
  └────────────────────────────────────────────┘

  块大小 [4M]:
```

- **块大小**：dd 每次读写的数据量，回车使用默认 4M

```
  压缩级别:
    0 = 不压缩 (局域网快速, 节省远程 CPU)
    1 = 最快 (默认, 适合日常备份)
    6 = 均衡 (中等压缩率)
    9 = 最小 (最高压缩, 费 CPU)
    回车使用默认值 1
  压缩级别 [1]:
```

- **压缩级别**：回车默认 1（最快），0 为不压缩

```
  压缩方式:
    1 = gzip (单核压缩, 兼容性最广)
    2 = pigz (多核压缩, 速度更快, 需远程安装)
  压缩方式 [1]:
```

- **压缩方式**：1 用 gzip（单核），2 用 pigz（多核并行，速度快 3-4 倍）

```
  传输前先零填充空闲空间?
    将空闲空间写零可大幅提高压缩率，减少网络传输量。
    可能需要较长时间，但能显著减少网络流量。
  零填充 (回车=执行填充, 输入n=跳过填充) [Y/n]:
```

- **零填充**：回车默认 Y（填充空闲空间，大幅提高压缩率），输入 n 跳过

```
  重建 initramfs (兼容不同硬件启动)?
    将远程系统的 initramfs 重建为包含所有硬件驱动
    这样备份镜像恢复到其他机器(不同CPU/硬盘控制器)时也能正常启动
    需要挂载远程分区并 chroot, 耗时约 1-2 分钟
  重建 initramfs [Y/n]:
```

- **重建 initramfs**：回车默认 Y（包含所有硬件驱动，跨硬件启动必需），输入 n 跳过

**第十步：确认并开始克隆**

> 如果目标盘小于源盘，程序会先警告"整盘克隆会被截断，恢复后目标文件系统将损坏"，需要输入 `yes` 强制确认——正常情况应换更大的目标盘。

```
  此操作将覆盖 /dev/sda 上的所有数据!
    确认开始克隆? 输入 yes 继续: yes

  开始克隆...

  [================>-----------------------]  45.2%  118.5 MB/s  18.1GB/40.0GB  用时: 2分35秒  ETA: 2分55秒
```

**第十一步：克隆完成 + 自动修复引导**

```
  克隆完成!
  修复引导（确保目标盘可启动）...
    Root: /dev/sda1
  -> dracut --no-hostonly --regenerate-all --force
  -> grub2-install --recheck /dev/sda
  ✓ GRUB reinstalled and initramfs rebuilt

  ===============================================
  全部完成! 总耗时: 12分34秒
  ===============================================
```

模式 1 完成后**自动重装 GRUB + 重建 initramfs + 修 fstab**（详见 [引导修复](#引导修复-grub--initramfs)），通常**直接重启即可启动**。

然后提示：

```
  继续其他操作? 输入 yes 继续，其他退出:
```

- 输入 `yes` → 回到操作模式选择，可以继续操作其他磁盘
- 输入其他或回车 → 按回车退出程序

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

dd 写物理磁盘，程序跑在内存，两者互不干扰。重启后内存清空（程序消失），物理硬盘上的克隆数据保留。

---

## 模式 2 — 保存为 gzip 文件

把远程磁盘保存为本地 `.img.gz` 压缩文件。支持备份单个磁盘或一次性备份全部磁盘。

### 数据流

```
远程: dd | gzip -N → SSH → 本地: 直接写入 .img.gz 文件
```

### 详细操作步骤

**第一步：源服务器进入 Alpine RAM OS**

```bash
bash reinstall.sh alpine --hold 1
```

**第二步：运行程序**

在本地机器（Linux 或 Windows）上运行：

```bash
./disk-cloner-linux-amd64
# 或 Windows:
disk-cloner-windows-amd64.exe
```

**第三步：选择操作模式 [2]**

程序启动后先选择操作模式（同模式 1 第四步的菜单，Windows 上只显示 [2] 和 [3]），输入 `2`。

**第四步：输入远程 SSH 信息**

同模式 1 第五步。

**第五步：选择源磁盘**

```
===============================================
  选择源磁盘 — 输入序号选定远程磁盘
===============================================
  [1] /dev/sda            30 GB
  [2] /dev/sdb            70 GB
  [0] 备份全部磁盘
  请输入序号:
```

- 输入序号（如 `1`）= 备份指定磁盘
- 输入 `0` = 一次性备份全部磁盘（共享同一组设置，逐个执行）
- 输入 `q` = 返回上一步

**第六步：选择保存目录**

```
  当前目录: /root
  ─────────────────────────────────────────────
  回车 → 使用当前目录
  输入 b → 浏览文件夹
  输入路径 → 保存到该路径
    保存目录 [.]:
```

- **回车** = 在当前目录下自动创建日期子目录（如 `2026-07-03/`）
- **输入路径** = 保存到指定目录（也会自动创建日期子目录）
- **输入 `b`**（仅 Windows）= 弹出文件夹浏览对话框

程序自动创建日期子目录：

```
  保存目录: /root/2026-07-03
```

**第七步：确认文件名**

```
  文件名 [/root/2026-07-03/192.168.1.100-sda-30G-2026-07-03.img.gz]:
```

默认格式：`IP-磁盘名-容量-日期.img.gz`。回车使用默认，或输入自定义名称。

**第八步：设置压缩参数**

同模式 1 第九步的压缩级别、压缩方式、零填充、重建 initramfs。

**第九步：确认并开始保存**

```
  确认开始保存? 输入 yes 继续: yes

  开始保存...

  Remote compressing (dd|gzip -> net -> file)
  [================>-----------------------]  45.2%  118.5 MB/s  18.1GB/40.0GB  用时: 2分35秒  ETA: 2分55秒
```

**第十步：保存完成**

```
  文件大小: 6.95 GB (压缩率 23.2%)
  校验文件: /root/2026-07-03/192.168.1.100-sda-30G-2026-07-03.img.gz.sha256

  ===============================================
  保存完成! 总耗时: 12分34秒
  ===============================================

  继续其他操作? 输入 yes 继续，其他退出:
```

### 输出目录结构

```
./2026-07-03/
├── 192.168.1.100-sda-30G-2026-07-03.img.gz
├── 192.168.1.100-sda-30G-2026-07-03.img.gz.sha256
├── 192.168.1.100-sda-30G-2026-07-03.img.gz.size
├── 192.168.1.100-sda-30G-2026-07-03.img.gz.log
├── 192.168.1.100-sdb-70G-2026-07-03.img.gz
├── 192.168.1.100-sdb-70G-2026-07-03.img.gz.sha256
├── 192.168.1.100-sdb-70G-2026-07-03.img.gz.size
└── 192.168.1.100-sdb-70G-2026-07-03.img.gz.log
```

每个镜像旁有 3 个附属文件：`.sha256` 校验、`.size` 解压后真实大小、`.log` 本次传输的完整日志（连接、参数、进度、结果，排查问题用）。

### 压缩包兼容性

生成的 `.img.gz` 是标准 gzip 格式（RFC 1952），可以在任何 Linux 上恢复：

> 使用 `-z 0`（不压缩）保存时输出原始 `.img` 文件（不是 gzip），恢复时程序会自动识别，也可以手动 `dd if=xxx.img of=/dev/sda` 恢复。

```bash
# 用标准 gunzip + dd 恢复
gunzip -c 192.168.1.100-sda-30G-2026-07-03.img.gz | dd of=/dev/sda bs=4M

# 或者先解压再 dd
gzip -d 192.168.1.100-sda-30G-2026-07-03.img.gz
dd if=192.168.1.100-sda-30G-2026-07-03.img of=/dev/sda bs=4M

# 验证 SHA256
sha256sum -c 192.168.1.100-sda-30G-2026-07-03.img.gz.sha256
```

### 备份全部磁盘

选择 `[0]` 后，程序一次性询问所有设置（保存目录、块大小、压缩级别、压缩方式、零填充、重建 initramfs），然后逐个磁盘执行备份：

```
  --- 备份 1/2: /dev/sda ---
  开始保存...
  [progress bar...]
  文件大小: 6.95 GB (压缩率 23.2%)
  校验文件: ...sda...sha256

  --- 备份 2/2: /dev/sdb ---
  开始保存...
  [progress bar...]
  文件大小: 15.2 GB (压缩率 21.7%)
  校验文件: ...sdb...sha256
```

---

## 模式 3 — 恢复文件到远程磁盘

把本地 `.img.gz` 备份恢复到远程硬盘。

### 数据流

```
本地: .img.gz → gunzip → SSH stdin → 远程: dd of=/dev/sda
```

### 详细操作步骤

**第一步：目标服务器进入 Alpine RAM OS**

```bash
bash reinstall.sh alpine --hold 1
```

**第二步：运行程序**

在本地机器上运行程序。

**第三步：选择操作模式 [3]**

程序启动后先选择操作模式（同模式 1 第四步的菜单，Windows 上只显示 [2] 和 [3]），输入 `3`。

**第四步：输入目标服务器的 SSH 信息**

同模式 1 第五步（IP 填目标服务器 B 的）。

**第五步：选择源磁盘**（用于默认目标磁盘提示）

**第六步：选择本地文件**

```
  本地文件路径 (Tab=补全, 回车=浏览, q=返回):
```

- **回车** = 弹出文件选择对话框（Windows）或浏览当前目录（Linux）
- **输入路径** = 直接指定文件路径
- **Linux 上按 Tab 补全路径**（与 shell 类似：唯一匹配直接补全，多个匹配先补全公共前缀、再按 Tab 列出候选；`~` 会展开为家目录）
- **路径输入错误不会退出**：提示"无法访问文件"后留在原地等待重新输入，输入 `q` 才返回上一步
- **Windows 上可拖拽文件到 cmd 窗口**，自动填入路径

浏览目录时显示：

```
  当前目录的镜像文件:
  [1] 192.168.1.100-sda-30G-2026-07-03.img.gz    6.95 GB
  [2] 192.168.1.100-sdb-70G-2026-07-03.img.gz    15.2 GB
  选择文件:
```

**第七步：确认远程目标磁盘**

```
  已选中文件: C:\Users\User\Downloads\192.168.1.100-sda-30G-2026-07-03.img.gz

  远程目标磁盘，确认请回车 [/dev/sda]:
```

回车使用默认（源磁盘路径），或输入其他磁盘路径。

**第八步：程序自动检查**

- **SHA256 校验**：如果存在 `.sha256` 文件，自动验证文件完整性
- **大小检查**：读取 gzip 尾部 ISIZE 获取解压后大小，与远程目标磁盘对比，目标过小时警告

```
  ✓ 文件完整性校验通过
```

或

```
  [!] 目标盘 (15 GB) 小于解压后镜像 (30 GB)，只能写入约 50.0%
  继续恢复? 输入 yes:
```

**第九步：确认并开始恢复**

```
  此操作将覆盖远程 /dev/sda 上的所有数据!
    确认开始恢复? 输入 yes 继续: yes

  开始恢复...

  [================>-----------------------]  45.2%  118.5 MB/s  18.1GB/40.0GB  用时: 2分35秒  ETA: 2分55秒
```

**第十步：恢复完成 + 自动修复引导**

```
  恢复完成! 总耗时: 12分34秒

  修复引导（GRUB + initramfs，确保目标机能启动）...
    Root: /dev/sda1
  -> dracut --no-hostonly
  -> grub2-install --recheck /dev/sda
  ✓ GRUB reinstalled and initramfs rebuilt

  正在验证目标文件系统一致性 (只读 fsck)...
  ✓ 目标文件系统一致性检查通过

  ===============================================
  全部完成! 总耗时: 12分58秒
  ===============================================

  继续其他操作? 输入 yes 继续，其他退出:
```

模式 3 完成后**自动重装 GRUB + 重建 initramfs + 修 fstab**（详见 [引导修复](#引导修复-grub--initramfs)），通常**直接重启目标机即可启动**。

---

## 引导修复 (GRUB + initramfs)

整盘 dd 复制后，目标盘**几乎总是需要修复引导**才能正常启动。常见症状：

```
GRUB error: unknown filesystem. Entering rescue mode...
grub rescue> _
```

或

```
VFS: Unable to mount root fs on unknown-block(0,0)
dracut:/# _
```

### 为什么 dd 后引导会坏

整盘 dd 只搬运数据，不会按目标机环境重装引导。dd 过去的镜像里有两样东西是**源机器专属**的：

1. **GRUB core.img**（MBR / BIOS Boot Partition 里的引导代码）
   - core.img 内部用块偏移直接定位 `/boot/grub2/grub.cfg`，这些偏移在 GRUB 安装时按当时磁盘的几何参数（磁头数、每磁道扇区数、分区起始位置）硬编码
   - 源盘是 virtio（vda）→ 目标盘是 SATA（sda），或源/目标盘容量不同导致分区表 gap 不同，core.img 里的偏移就读不到正确位置 → `unknown filesystem`
   - 即使能读到，core.img 内置的 xfs/ext4 模块也只针对源磁盘上的文件系统元数据版本

2. **initramfs**（启动早期根文件系统）
   - 默认 hostonly 模式只打包**当前硬件的驱动**（云服务器通常只有 virtio 驱动）
   - 恢复到 VMware / Hyper-V / KVM-SATA 后，内核找不到磁盘（virtio 驱动不匹配）→ `dracut emergency` / `VFS unable to mount root fs`

### 自动修复（默认行为）

**模式 1（克隆）、模式 3（恢复）完成后，程序会自动执行引导修复**，无需任何手动步骤。修复日志：

```
  修复引导（GRUB + initramfs，确保目标机能启动）...
    Root: /dev/sda1
  -> dracut --no-hostonly
  -> grub2-install --recheck /dev/sda
  ✓ GRUB reinstalled and initramfs rebuilt
```

具体做的事：

1. `mdev -s` 扫描并创建分区设备节点（Alpine 精简环境需要）
2. `lvm vgscan + vgchange -ay` 激活 LVM 卷组（如有）
3. 通过 `blkid` 找到根分区（支持 ext4 / xfs / btrfs），自动检测 fstab 单独挂载 `/boot` 和 `/boot/efi`
4. bind mount `/dev /proc /sys` + tmpfs `/run`
5. chroot 进系统，根据发行版自动选工具：
   - `dracut --no-hostonly --force --regenerate-all`（Fedora / RHEL / CentOS / Rocky / Alma）
   - `update-initramfs -u -k all`（Debian / Ubuntu）
   - `mkinitcpio -P`（Arch / Manjaro）
6. `grub2-install --recheck <目标磁盘>`（或 `grub-install`）重装 GRUB
7. `grub2-mkconfig -o /boot/grub2/grub.cfg`（或 `grub-mkconfig`）重新生成菜单
8. 修复 `/etc/fstab`：把目标机上不存在的额外数据盘挂载条目自动注释掉，备份原文件为 `fstab.bak`

> **注意**：保存模式（模式 2）的 `重建 initramfs` 选项**只**重建 initramfs（在源机执行），不重装 GRUB——因为 GRUB 要装到目标盘，而模式 2 还没有目标盘。模式 1 / 模式 3 才会装 GRUB。

### 独立模式：手动修复已经坏掉的盘

如果之前用旧版本恢复后已经无法启动（停在 `grub rescue>`），不必重新 dd，**进 Alpine RAM OS 直接修复即可**：

```bash
# 1. 让目标机进 Alpine RAM OS
bash reinstall.sh alpine --hold 1

# 2. SSH 进去后运行独立修复模式
./disk-cloner-linux-amd64 --fix-boot-disk /dev/sda
```

输出示例：

```
  修复引导 - 独立模式
  刷新分区表...
  检测 LVM...
  查找根文件系统...
    根分区: /dev/sda1 (xfs)
    系统类型: centos
  挂载虚拟文件系统...
  重建 initramfs (包含所有硬件驱动)...
    -> dracut --no-hostonly --regenerate-all --force
  修复 GRUB 引导...
    检测到 BIOS/Legacy 模式
    -> grub2-install --recheck /dev/sda
    -> grub2-mkconfig -o /boot/grub2/grub.cfg
  修复 fstab (移除不存在的额外磁盘挂载)...
  清理挂载点...
  ✓ 引导修复完成!
```

修复完后 `reboot` 即可正常启动。

### 完全手动修复（不想用本程序时）

也可以用 CentOS / Rocky / Ubuntu 安装 ISO 进入 rescue 模式，手动 chroot 修复：

```bash
# 假设救援模式已把系统挂到 /mnt
mount --bind /dev /mnt/dev
mount --bind /proc /mnt/proc
mount --bind /sys /mnt/sys
chroot /mnt

# CentOS / Rocky / Alma / Fedora
dracut --no-hostonly --force --regenerate-all
grub2-install --recheck /dev/sda
grub2-mkconfig -o /boot/grub2/grub.cfg

# Debian / Ubuntu
update-initramfs -u -k all
grub-install --recheck /dev/sda
update-grub

exit; reboot
```

### BIOS vs UEFI

修复时程序会自动检测 `/boot/efi/EFI` 目录是否存在：
- **存在** = UEFI 模式 → `grub2-install --target=x86_64-efi --efi-directory=/boot/efi` + 用 `efibootmgr` 添加 UEFI 启动项
- **不存在** = BIOS / Legacy 模式 → `grub2-install --recheck <磁盘>`

**前提**：源机器和目标机的启动模式要一致。CentOS 7 / 旧版云服务器多数是 BIOS；现代 UEFI 服务器恢复到 BIOS 虚拟机（或反过来）即使修复了 GRUB 也不能启动。可用 `ls /sys/firmware/efi` 检查：存在目录 = UEFI，不存在 = BIOS。

---

## 模式 4 — 传输到远程存储

把本机硬盘的 dd 流直接推到远程存储服务（SFTP / FTP / WebDAV / S3 兼容对象存储 / Pixeldrain 网盘），**镜像不占用任何本地磁盘空间**。程序运行在被克隆的服务器上（Alpine RAM OS），本机硬盘只读，数据在内存中压缩后即刻上传。仅 Linux。

### 数据流

```
本机 = 源服务器 (Alpine RAM OS)                                 远程存储
┌──────────────────────────────────────────────┐
│  dd if=/dev/sda | gzip -1                    │  ──▶  SFTP / FTP /
│  SHA256 同步计算 (程序在内存里运行, 硬盘只读)   │   WebDAV / S3 / Pixeldrain
└──────────────────────────────────────────────┘
```

### 操作方式（二选一）

- **交互模式**：启动程序后选操作模式 `[4] 读取本地硬盘传输到远程存储`，选中本机磁盘，再按提示填存储信息即可
- **命令行模式**：

```bash
# 在源服务器上直接运行 (已进入 Alpine RAM OS)
./disk-cloner -l /dev/sda -dst 'sftp://user:pass@nas.lan/backup/' -y
./disk-cloner -l /dev/sda -dst 's3://AKID:SECRET@minio.lan:9000/bkt/img.gz?path=1' -y
./disk-cloner -l /dev/sda -dst 'pixeldrain://:APIKEY@pixeldrain.com/backup.img.gz' -y
```

零填充、重建 initramfs、SHA256 校验、失败清理等行为与模式 2 一致。

> Pixeldrain URL 里的密码位就是 API Key（用户名位留空），`pd://` 是 `pixeldrain://` 的别名；上传成功后会打印 `https://pixeldrain.com/u/<id>` 分享链接和直链下载地址。大小限制、保留策略、隐私注意事项见下方 [Pixeldrain 详解](#pixeldrain-详解)。

> 程序必须运行在 Alpine RAM OS：启动后会先检测本机根文件系统，不是 tmpfs/overlay 时会要求确认。

> 高级用法：命令行 `-dst` 也可以搭配 `-H/-s` 在一台中转机上运行（通过 SSH 读取远端磁盘后转发给存储），适合脚本化批量备份：
> `disk-cloner -H 192.168.1.100 -p password -s /dev/sda -dst 'sftp://user:pass@nas.lan/backup/' -y`

### 支持的存储类型

| 类型 | 说明 |
|------|------|
| SFTP | SSH 文件传输（推荐）。密码或本地密钥认证；自动创建缺失的父目录 |
| FTP | 内置 FTP 客户端（EPSV/PASV 被动模式，自动 MKD 建目录） |
| WebDAV | HTTP PUT。自动探测服务器是否支持分块（chunked）上传；不支持时（如 nginx dav 模块）自动回退本地暂存后再 PUT |
| S3 | AWS S3 及所有 S3 兼容存储（MinIO、Ceph 等）。手写 SigV4 签名 + 分片上传（起始 8 MiB，大镜像自动加倍到 256 MiB），支持虚拟主机与 path-style 两种寻址 |
| Pixeldrain | 网盘直传（pixeldrain.com）。上传成功后直接给出分享链接，适合没有自己的存储服务器、想把镜像发给别人或存到云端的场景。需要 API Key（账户设置页生成），不支持匿名上传。有单文件大小限制和保留策略，敏感镜像慎用，详见 [Pixeldrain 详解](#pixeldrain-详解) |

### Pixeldrain 详解

Pixeldrain（pixeldrain.com）是公共网盘。模式 4 把它作为一种存储后端：适合**没有自己的存储服务器**、想把镜像直接发给别人下载、或临时存到云端的场景。上传成功后程序会直接打印分享链接和直链下载地址：

```
分享链接: https://pixeldrain.com/u/<文件ID>
直链下载: https://pixeldrain.com/u/<文件ID>?download
```

**获取 API Key**：登录 pixeldrain.com → 头像菜单 → Account Settings（账户设置）→ 生成并复制 API Key。Pixeldrain 不支持匿名上传，API Key 必填。

**URL 写法**（密码位就是 API Key，用户名位留空；`pd://` 是 `pixeldrain://` 的别名）：

```bash
disk-cloner -l /dev/sda -dst 'pixeldrain://:APIKEY@pixeldrain.com/myserver-sda.img.gz' -y
disk-cloner -l /dev/sda -dst 'pd://APIKEY@pixeldrain.com/myserver-sda.img.gz' -y
```

文件名只是 Pixeldrain 上的显示名称（没有目录概念），交互模式里直接回车即可使用自动命名的文件名。

**使用前必读的限制**：

| 事项 | 说明 |
|------|------|
| 单文件大小限制 | 免费账户目前约 20 GB（政策时有调整，以官方说明为准）。**超限时服务器要等整个文件接收完才拒绝（413 错误）**——即传了几个小时后才报错，无法提前检测，所以上传前请先确认压缩后的镜像大小在限额内 |
| 保留策略 | 免费账户的文件 **60 天无人访问会被删除**（被下载/查看则重新计时），适合中转分享，不适合当唯一备份存放 |
| 隐私 | 任何人拿到链接（文件 ID）就能下载，链接没有密码保护。整盘镜像包含服务器全部数据，**敏感镜像不要直传**：如需上云请自行先加密（本工具不提供加密功能），或改用 SFTP / S3 等私有存储 |
| 传输特性 | 单请求流式上传，**无断点续传**，中断后只能重新上传。HTTPS 证书校验默认关闭（与 WebDAV/S3 一致，可加 `tlsverify=1` 或 `-tls-verify` 开启） |

### 传输前自动校验

连接存储服务器在零填充**之前**进行——账号密码错误、路径/桶不可写、权限不足会在几秒内报错返回，不会白白跑完几小时的零填充才发现存不了。

### 传输失败自动清理

传输中断（Ctrl+C）或失败时，程序会清理远端残留：SFTP/FTP 删除半截文件，WebDAV 发送 DELETE，S3 中止（Abort）整个分片上传任务，Pixeldrain 直接掐断上传（服务端收完整个文件前不会生成文件，因此没有残留需要清理）。本地只会留下小的 `.sha256`、`.size`、`.log` 校验文件，与镜像文件分开保存（默认在当前目录的日期子目录里），下载镜像后可以用来校验完整性。

### 操作步骤（交互模式）

1. 启动程序（已运行在源机的 Alpine RAM OS 上），选择操作模式 `[4] 读取本地硬盘传输到远程存储`
2. 选择本机源磁盘
3. 选择存储类型并填入连接信息（IP/端口/用户名/密码等）
4. 设置块大小、压缩级别、压缩方式、零填充、重建 initramfs（与模式 2 相同）
5. 确认目标路径（SFTP/FTP 为远程路径，WebDAV 为完整 URL，S3 为对象 Key，Pixeldrain 为文件名），回车使用自动命名
6. 选择本地校验文件目录
7. 确认后开始传输，进度条显示的是解压后的磁盘字节数

---

## 命令行模式

无需交互，直接通过参数执行操作。

### 示例

```bash
# 克隆远程磁盘到本地
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -t /dev/sda -y

# 保存为文件（自动命名+日期目录）
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -o auto -y

# 恢复文件到远程磁盘
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -r backup.img.gz -y

# 传输到远程存储（本机模式：在源服务器上直接运行, 需已进入源机 RAM OS）
disk-cloner -l /dev/sda -dst 'sftp://user:pass@nas.lan/backup/' -y

# 传输到 S3 兼容对象存储（path-style, MinIO）
disk-cloner -l /dev/sda -dst 's3://AKID:SECRET@minio.lan:9000/bkt/img.gz?path=1' -y

# 传输到 Pixeldrain 网盘（上传成功后输出分享链接）
disk-cloner -l /dev/sda -dst 'pixeldrain://:APIKEY@pixeldrain.com/img.img.gz' -y

# 高级：在一台中转机上通过 SSH 读取远端磁盘后转发给存储（脚本化批量备份）
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -dst 'sftp://user:pass@nas.lan/backup/' -y

# 本机模式：在源服务器上直接把本机硬盘传到远程存储（需已在源机 RAM OS 中）
disk-cloner -l /dev/sda -dst 'sftp://user:pass@nas.lan/backup/' -y

# 最高压缩
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -o auto -z 9 -y

# 不压缩（局域网快速传输）
disk-cloner -H 192.168.1.100 -p password -s /dev/sda -o auto -z 0 -y
```

### 参数表

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-H` | 远程服务器 IP | — |
| `-P` | SSH 端口 | 22 |
| `-u` | SSH 用户名 | root |
| `-p` | SSH 密码（不提供则尝试密钥认证；也可用环境变量 `DISK_CLONER_PASSWORD`，避免密码进入 shell 历史） | — |
| `-s` | 源磁盘路径（远程），如 `/dev/sda` | — |
| `-t` | 目标磁盘路径（本地），如 `/dev/sda` | — |
| `-o` | 保存为镜像文件，`auto` 自动命名+日期目录（`-z 0` 时为原始 `.img`，否则 gzip `.img.gz`） | — |
| `-r` | 从 gzip 文件恢复到远程磁盘 | — |
| `-dst` | 传输到远程存储，镜像不落本地磁盘。支持 `sftp://` `ftp://` `dav://` `davs://` `s3://` `pixeldrain://`（别名 `pd://`），URL 格式见 [模式 4](#模式-4--传输到远程存储) | — |
| `-l` | 本机磁盘作为源（程序直接运行在源机 RAM OS，仅 Linux；搭配 `-dst` 或 `-o`） | — |
| `-bs` | dd 块大小 | 4M |
| `-z` | 压缩级别 0-9（0=不压缩，1=最快，9=最小） | 1 |
| `-y` | 跳过确认提示 | false |
| `-V` | 显示版本号 | — |
| `--fix-boot-disk <磁盘>` | 独立修复引导：chroot 进系统重装 GRUB + 重建 initramfs + 修 fstab（详见 [引导修复](#引导修复-grub--initramfs)） | — |
| `--no-fix-boot` | 命令行克隆（`-t`）和恢复（`-r`）时跳过自动引导修复（交互模式不受影响） | — |
| `-tls-verify` | 对 WebDAV/S3/Pixeldrain 的 HTTPS 启用证书校验（默认关闭；`-dst` URL 中也可加 `tlsverify=1` 参数） | false |

### 独立修复引导

```bash
# 在已进入 Alpine RAM OS 的目标机上，直接修复磁盘引导
./disk-cloner-linux-amd64 --fix-boot-disk /dev/sda
```

### 密码含特殊字符

密码中有 `$`、`~`、`?`、`%` 等特殊字符时，命令行需用单引号：

```bash
./disk-cloner -H 192.168.1.100 -p 'kI$~4)Tz?%E5ai78' -s /dev/sda -o auto -y
```

---

## 关键功能详解

### 零填充

零填充在 dd 之前对远程磁盘的每个可挂载分区写入全零，把空闲空间变成连续零字节。gzip 对连续零的压缩率极高（接近 1000:1）。

**效果对比**：

```
100 GB 磁盘，实际数据 14 GB
  不零填充: 压缩包 ~50-90 GB（空闲空间残留数据压不动）
  零填充后: 压缩包 ~14 GB（空闲空间全零，极高压缩率）
```

**LVM 支持**：自动安装 `lvm2`，`vgscan + vgchange -ay` 激活卷组后挂载 LVM 逻辑卷并填充。

**进度显示**：零填充过程中每 5 秒报告已写入数据量：

```
    Filling: /dev/mapper/J3160--vg-root
      /dev/mapper/J3160--vg-root: 5.00 GB written so far
      /dev/mapper/J3160--vg-root: 10.0 GB written so far
    Done: /dev/mapper/J3160--vg-root
```

### initramfs 重建（备份前选项）

**模式 2 保存**前的"重建 initramfs [Y/n]"选项，在备份前到源系统 chroot 一次，让 initramfs 包含**所有硬件驱动**（virtio、nvme、各种 SCSI/SATA 控制器、ext4/xfs/btrfs 文件系统驱动等）。

> 此选项**只重建 initramfs，不重装 GRUB**——因为模式 2 还没有目标盘。GRUB 的重装在**模式 1 / 模式 3** 恢复时自动做，详见 [引导修复](#引导修复-grub--initramfs)。

**原理**：`dracut --no-hostonly` 把 `/lib/modules/` 下所有内核模块都打包进 initramfs，而不是只包含当前硬件的驱动（hostonly 模式）。

**为什么需要**：云服务器的 initramfs 默认只包含 virtio 驱动。恢复到 VMware/Hyper-V 虚拟机后，virtio 驱动不匹配，内核找不到磁盘，报 `UUID does not exist`，无法启动。

**支持的工具（自动检测）**：

| 发行版 | 工具 | 命令 |
|--------|------|------|
| Fedora / RHEL / CentOS / Rocky / Alma | dracut | `dracut --no-hostonly --force --regenerate-all` |
| Debian / Ubuntu / Kali | update-initramfs | `update-initramfs -u -k all` |
| Arch / Manjaro | mkinitcpio | `mkinitcpio -P` |

**也可以在进入 Alpine RAM OS 之前，在正常运行的系统上手动执行**：

```bash
# Fedora / RHEL / CentOS
dracut --no-hostonly --force --regenerate-all

# Debian / Ubuntu
update-initramfs -u -k all
```

提前执行一次后，之后所有的备份都是通用镜像，不需要每次重复。

### SHA256 校验

- **保存时**：传输过程中同步计算 SHA256 并生成 `.sha256` 校验文件（无需保存后再重读整个文件），同时生成 `.size` 文件记录解压后真实大小
- **恢复时**：如果存在 `.sha256` 文件，自动验证文件完整性；目标盘容量检查优先读取 `.size`（gzip 头部的 ISIZE 超过 4GB 会回绕，不可靠）
- 校验通过 → 显示 `✓ 文件完整性校验通过`
- 校验失败 → 提示是否强制恢复
- 无校验文件 → 静默跳过
- 保存中断/失败时，未完成的镜像会重命名为 `.partial`，避免误当成有效备份

### 压缩方式

| 方式 | 说明 | 速度 | 兼容性 |
|------|------|------|--------|
| gzip | 单核压缩 | 较慢 | 所有系统自带 |
| pigz | 多核并行压缩 | 快 3-4 倍 | 需远程安装 `apk add pigz` |
| 不压缩 (-z 0) | 裸 dd 传输（保存模式输出原始 `.img` 文件，恢复时自动识别） | 局域网最快 | 无需压缩工具 |

- **gzip 和 pigz 产出的 .img.gz 格式完全相同**，可以互相解压
- 程序自动检测远程 CPU 核心数，pigz 使用 `pigz -N -p 核心数`
- 如果 pigz 安装失败，自动回退到 gzip

### 进度显示

每秒刷新实时进度，使用 30 秒滑动窗口平均速度计算稳定 ETA：

```
  [================>-----------------------]  45.2%  118.5 MB/s  18.1GB/40.0GB  用时: 2分35秒  ETA: 2分55秒
```

**窗口最小化不影响**：Windows 上异步刷新，最小化到任务栏后数据继续传输，进度恢复前台后正常显示。

**低速警告**：连续 30 秒平均速度低于 1 MB/s 时提示检查网络或远程 CPU 负载。

**SSH 断线检测**：每秒检测 SSH 连接状态，远程服务器重启/关机时秒级报错：

```
  SSH connection lost — remote host may have rebooted or shut down
```

### 日期目录

保存时自动创建日期子目录（格式 `年-月-日`，如 `2026-07-03`），文件名包含 IP、磁盘名、容量、日期：

```
./2026-07-03/192.168.1.100-sda-30G-2026-07-03.img.gz
```

### 备份全部磁盘

模式 2 选择 `[0]` 可一次性备份远程所有磁盘：

- 共享同一组设置（保存目录、压缩级别、压缩方式、零填充、重建 initramfs）
- 逐个磁盘执行备份，每个磁盘独立显示进度
- 每个磁盘生成独立的 `.img.gz` 和 `.sha256` 文件

### IP 自动提取

输入服务器 IP 时，如果复制了带其他文字的内容，程序自动提取其中的 IPv4 地址：

```
  服务器IP: IP: 192.168.1.100:22
  → 自动提取为 192.168.1.100
```

### 交互菜单返回

所有选择菜单输入 `q` 可返回上一步：

- 启动时的操作模式选择 → 输入 `q` → 退出程序
- "继续其他操作"后再次出现的操作模式菜单 → 输入 `q` → 回到 SSH 配置（可改连其他服务器）
- 源磁盘选择 → 输入 `q` → 回到操作模式
- 目标磁盘选择 → 输入 `q` → 回到源磁盘选择
- 文件选择 → 输入 `q` → 返回上一步

### 完成后继续操作

所有模式完成后不再直接退出，而是提示：

```
  继续其他操作? 输入 yes 继续，其他退出:
```

- 输入 `yes` → 回到操作模式选择，SSH 连接保持
- 输入其他或回车 → 按回车退出

---

## 克隆后必须做的事

**模式 1（克隆）和模式 3（恢复）**完成后，程序会**自动执行引导修复**（重装 GRUB + 重建 initramfs + 注释 fstab 中不存在的磁盘挂载），通常**直接重启即可启动**。

下面列出几种仍需手动处理的情况：

### 1. 扩分区用上更大的目标盘

整盘 dd 后分区大小还是源盘的，目标盘剩余空间未分配。例如 30GB 镜像恢复到 1TB 盘上，需要手动扩展：

```bash
# 扩展分区表里的分区（第 1 个分区）
growpart /dev/sda 1

# 扩展文件系统（xfs 用 xfs_growfs，ext4 用 resize2fs）
xfs_growfs /              # xfs（CentOS 7 默认）
# resize2fs /dev/sda1     # ext4

# 验证
df -h /
```

### 2. 目标盘比源盘小（罕见，且会截断数据）

恢复时程序会警告 `目标盘 (xx) 小于解压后镜像 (xx)`，dd 会被截断。重启前需修复 GPT 备份分区表：

```bash
# GPT 分区表（程序自动检测，备份头在盘尾被截断时使用）
sgdisk -e /dev/sda
partprobe /dev/sda

# 修复文件系统
xfs_repair /dev/sda1       # xfs
# e2fsck -fy /dev/sda1     # ext4
```

### 3. 启动模式不匹配（BIOS ↔ UEFI）

源机器是 BIOS、目标机是 UEFI（或反过来）时，自动修复也救不了。**必须让目标虚拟机的启动模式与源一致**：

```bash
# 检查源机器的启动模式
ls /sys/firmware/efi
# 存在目录 = UEFI；报 No such file = BIOS
```

虚拟机平台的对应设置：

| 平台 | BIOS 模式 | UEFI 模式 |
|------|----------|-----------|
| VMware | 默认 | Firmware = EFI |
| Hyper-V | Generation 1 VM | Generation 2 VM |
| VirtualBox | 默认 | Settings → System → Motherboard → Enable EFI |
| PVE / KVM | 默认 (SeaBIOS) | OVMF |
| 云厂商 | 一般默认 | 通常不支持自定义 |

### 4. fstab 仍卡 90 秒（自动修复失败时）

如果自动修复没注释干净 fstab（比如 fstab 用了非标准格式），重启后 systemd 会卡 90 秒等不存在的设备，然后进 emergency mode。手动处理：

```bash
# 进 Alpine RAM OS 或救援模式
mount /dev/sda1 /mnt        # 按实际根分区
vi /mnt/etc/fstab
# 注释掉 /data、/mnt/* 等不存在的磁盘条目（程序已备份原文件为 fstab.bak）
umount /mnt
reboot
```

---

## 平台说明

### Linux

- 程序启动时自动 `apk add util-linux lvm2 efibootmgr`
- 三种模式全支持（克隆、保存、恢复），外加远程存储传输（模式 4）
- **本机模式**（`-l` 参数，或交互模式选 `[4]`）：程序运行在源机 Alpine RAM OS 上直接读本机硬盘推送到远程存储。模式 1/2/3 的流程不受影响
- 输入使用 terminal raw 模式，退格/Ctrl+U/Ctrl+W 完整支持
- 文件路径输入支持 Tab 补全
- 默认输入值预填充到输入框中，可直接编辑

### Windows

- **支持保存 [2] 和恢复 [3]**（不克隆硬盘、不支持本机传输 [4]——这两项需要 Linux 块设备）
- 不需要安装 SSH 或 gzip — Go 程序已内置
- 恢复时回车弹出文件选择对话框，支持拖拽文件到 cmd 窗口
- 保存时回车弹出文件夹浏览对话框
- 密码输入为隐藏回显（不显示在屏幕上）
- 程序自动设置 UTF-8 编码（`chcp 65001`），中文路径正常
- 最小化到任务栏不影响传输（异步刷新，数据继续传输）
- 完成后按回车退出，不会闪退

---

## 技术架构

| 组件 | 实现 | 运行位置 |
|------|------|---------|
| SSH 协议 | Go `crypto/ssh`（纯 Go，不依赖系统 ssh） | 客户端 |
| gzip 压缩 | 远程 busybox gzip / pigz | 远程服务器 |
| gzip 解压 | Go `compress/gzip` | 客户端（恢复模式） |
| dd 读写 | 远程 busybox dd | 远程服务器 |
| 磁盘扫描 | 远程 `lsblk`（util-linux） | 远程服务器 |
| 文件对话框 | PowerShell WinForms | Windows 客户端 |
| 文件校验 | SHA256 | 客户端 |
| 引导修复（远程） | chroot + dracut / update-initramfs / mkinitcpio + grub2-install + grub2-mkconfig + fstab 清理 | 远程服务器（模式 1/3 自动执行） |
| 引导修复（本地） | 同上，单独模式 `--fix-boot-disk` | 本地（目标机进 RAM OS 后） |
| 远程存储（模式 4） | SFTP（pkg/sftp）、FTP（内置客户端）、WebDAV（HTTP PUT + 分块探测）、S3（手写 SigV4 分片上传）、Pixeldrain（流式 PUT + API Key 预检） | 本地客户端（流式转发，不落盘） |
| 文件系统一致性检查 | blkid 检测类型 + fsck.ext4 -fn（仅 ext 家族；xfs/btrfs 跳过） | 远程服务器 |
| LVM 支持 | vgscan + vgchange -ay | 远程服务器 |
| 进度刷新 | 异步 Sync，窗口最小化不影响 | 客户端 |
| 控制台编码 | `chcp 65001`（UTF-8） | Windows 客户端 |
| busybox dd 兼容 | bs=4M → bs=4194304（自动转换） | 远程服务器 |

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

---

## 自动发布

推送版本标签触发 GitHub Actions 自动编译并上传 Release：

```bash
git tag v1.0.0
git push origin v1.0.0
```

双击 `push.bat` 快速提交代码（自动 add/commit/push，支持打 tag）。

---

## 常见问题

### Q: 备份的镜像恢复到虚拟机后不能启动？

A: 检查恢复日志末尾。**新版程序在模式 1 / 模式 3 恢复完成后会自动重装 GRUB 和重建 initramfs**（日志会有 `修复引导（GRUB + initramfs）...`）。如果你用的是旧版本，或自动修复失败，请用独立修复：

```bash
# 在目标机进 Alpine RAM OS 后
./disk-cloner-linux-amd64 --fix-boot-disk /dev/sda
```

详见 [引导修复](#引导修复-grub--initramfs)。

### Q: 恢复后报 `GRUB error: unknown filesystem / grub rescue>`？

A: 这是 GRUB **第一阶段**错误，core.img 读不到 `/boot` 所在分区。原因：

1. **跨总线迁移**：源盘是 virtio（vda）、目标盘是 SATA（sda），GRUB core.img 里的块偏移失配。**这是正常的**，新版程序在恢复时会自动 `grub2-install` 重装 GRUB，无需手动处理。
2. **旧版本程序**：升级到新版后用 `--fix-boot-disk` 修复即可。
3. **启动模式不一致**：源机器 BIOS、目标机 UEFI（或反之）。检查 `ls /sys/firmware/efi`，让源/目标机模式一致。

### Q: 恢复后报 `VFS: Unable to mount root fs` 或 `dracut emergency`？

A: 这是内核**第二阶段**错误，initramfs 里没有目标机的磁盘驱动。原因：源机器的 initramfs 是 hostonly 模式，只含 virtio 驱动，目标机是 SATA/SCSI/NVMe 时识别不了。**新版程序会自动 `dracut --no-hostonly --force --regenerate-all`** 解决。如果是旧版备份文件，恢复时新版程序会重新执行这一步。

### Q: 恢复后 fsck 提示 `bad magic number in super-block`？

A: 这是**误报**。旧版程序对所有分区硬跑 `fsck.ext4`，遇到 xfs/btrfs 分区必然报这个错——并不是真的损坏。新版程序会先用 `blkid` 检测文件系统类型，xfs/btrfs 静默跳过。

如果担心 xfs 真的损坏（备份时源机器没进 RAM OS），手动验证：

```bash
# 进 Alpine RAM OS
xfs_repair -n /dev/sda1    # 只读检查
# 报错再修复：
xfs_repair /dev/sda1
```

### Q: 零填充很慢怎么办？

A: 零填充需要写入全部空闲空间，磁盘 IO 决定速度。如果局域网带宽足够（千兆），可以选择跳过零填充（输入 n），直接传原始数据。不零填充压缩率差但传输不需要等填充。

### Q: 目标盘比源盘小怎么办？

A: 恢复时程序会检测目标盘大小并警告。如果实际数据量小于目标盘容量，可以继续恢复，但需要恢复后手动修复 GPT 分区表（`sgdisk -e /dev/sda`）。

### Q: 目标盘比源盘大，多余空间怎么用？

A: 整盘 dd 后分区大小还是源盘的，目标盘剩余空间未分配。扩展：

```bash
growpart /dev/sda 1
xfs_growfs /              # xfs
# resize2fs /dev/sda1     # ext4
```

### Q: 压缩包太大怎么办？

A: 1) 确保选了零填充（Y）。2) 提高压缩级别（-z 6 或 -z 9）。3) 使用 pigz 多核压缩（压缩方式选 2）。

### Q: SSH 连接失败？

A: 1) 确认 IP 和端口正确。2) 确认远程服务器已进入 Alpine RAM OS。3) 确认密码正确（特殊字符用单引号包裹）。4) 程序有密码时不加载本地 SSH 密钥，避免超出服务器 MaxAuthTries 限制。5) 密码也可通过环境变量 `DISK_CLONER_PASSWORD` 提供，避免留在 shell 历史里。

### Q: 主机密钥安全吗？

A: 程序不保存/校验主机密钥——远程每次重启进入 Alpine RAM OS 都会重新生成主机密钥，固定指纹会导致每次连接都报错。作为补偿，连接成功后会显示服务器主机密钥的 SHA256 指纹，可在可信网络中与服务器上的 `/etc/ssh/ssh_host_*_key.pub` 核对。请勿在不可信网络（公共 Wi-Fi 等）中使用本工具传输。

### Q: WebDAV / S3 / Pixeldrain 的 HTTPS 证书校验是关闭的吗？

A: 默认关闭（自建 NAS / MinIO 普遍使用自签名证书，开启会导致全部连接失败）。需要在不可信网络中启用时，加 `-tls-verify` 参数，或在 `-dst` URL 后附加 `tlsverify=1`（如 `davs://user:pass@nas.lan:5006/dav/img.img.gz?tlsverify=1`，Pixeldrain 同样支持）。无论哪种方式，都建议配合 HTTPS 使用，避免 Basic Auth 凭据明文暴露。

### Q: 模式 4 的存储类型怎么选？

A: 按手头有的资源选：

- **有 NAS / Linux 服务器** → SFTP（推荐）：稳定、支持密码或密钥认证、自动建目录
- **有对象存储（AWS S3、MinIO、Ceph 等）** → S3：分片上传，适合大镜像长期归档
- **NAS 开了 WebDAV（群晖/威联通/坚果云等）** → WebDAV
- **什么存储服务器都没有，或想把镜像直接发给别人下载** → Pixeldrain 网盘直传，上传成功即得分享链接。但注意：免费账户单文件约 20 GB 上限、60 天无人访问会被删除、拿到链接的人都能下载——敏感镜像请勿直传（详见 [Pixeldrain 详解](#pixeldrain-详解)）

### Q: 远程服务器磁盘名不是 sda？

A: 程序自动扫描并列出所有磁盘（sda、vda、nvme0n1 等），选择对应的序号即可。整盘 dd 后磁盘名可能改变（源 vda → 目标 sda），这是正常的——引导修复会按目标磁盘名重装 GRUB。

---

## License

MIT
