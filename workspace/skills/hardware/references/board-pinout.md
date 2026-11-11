# 开发板引脚定义与引脚复用参考

## LicheeRV Nano (SG2002)

### I2C 总线

| 总线 | 引脚 | 说明 |
|-----|------|-------|
| I2C-1 | P18 (SCL), P21 (SDA) | **与 WiFi SDIO 共用** —— 必须先停止 WiFi |
| I2C-3 | 排针上可用 | 查阅设备树以确认引脚分配 |
| I2C-5 | 软件模拟（BitBang） | 速度较慢但不存在引脚冲突 |

### SPI 总线

| 总线 | 引脚 | 说明 |
|-----|------|-------|
| SPI-2 | P18 (CS), P21 (MISO), P22 (MOSI), P23 (SCK) | **与 WiFi 共用** —— 必须先停止 WiFi |
| SPI-4 | 软件模拟（BitBang） | 速度较慢但不存在引脚冲突 |

### I2C-1 配置步骤

```bash
# 1. 停止 WiFi（与 I2C-1 共用引脚）
/etc/init.d/S30wifi stop

# 2. 为 I2C-1 配置引脚复用
devmem 0x030010D0 b 0x2   # P18 → I2C1_SCL
devmem 0x030010DC b 0x2   # P21 → I2C1_SDA

# 3. 加载 i2c-dev 模块
modprobe i2c-dev

# 4. 验证
ls /dev/i2c-*
```

### SPI-2 配置步骤

```bash
# 1. 停止 WiFi（与 SPI-2 共用引脚）
/etc/init.d/S30wifi stop

# 2. 为 SPI-2 配置引脚复用
devmem 0x030010D0 b 0x1   # P18 → SPI2_CS
devmem 0x030010DC b 0x1   # P21 → SPI2_MISO
devmem 0x030010E0 b 0x1   # P22 → SPI2_MOSI
devmem 0x030010E4 b 0x1   # P23 → SPI2_SCK

# 3. 验证
ls /dev/spidev*
```

### 实测最高 SPI 速度
- SPI-2 硬件：实测最高可达 **93 MHz**
- 官方镜像已预装 `spidev_test`，可用于回环测试

---

## MaixCAM

### I2C 总线

| 总线 | 引脚 | 说明 |
|-----|------|-------|
| I2C-1 | 与 WiFi 引脚重叠 | 不推荐 |
| I2C-3 | 与 WiFi 引脚重叠 | 不推荐 |
| I2C-5 | A15 (SCL), A27 (SDA) | **推荐** —— 软件 I2C，无冲突 |

### I2C-5 配置步骤

```bash
# 使用 pinmap 工具配置引脚
# （MaixCAM 使用 pinmap 工具而非 devmem）
# 参见：https://wiki.sipeed.com/hardware/en/maixcam/gpio.html

# 加载 i2c-dev
modprobe i2c-dev

# 验证
ls /dev/i2c-*
```

---

## MaixCAM2

### I2C 总线

| 总线 | 引脚 | 说明 |
|-----|------|-------|
| I2C-6 | A1 (SCL), A0 (SDA) | 排针上可用 |
| I2C-7 | 可用 | 查阅设备树 |

### 配置步骤

```bash
# 为 I2C-6 配置 pinmap
# A1 → I2C6_SCL，A0 → I2C6_SDA
# 引脚映射命令参见 MaixCAM2 文档

modprobe i2c-dev
ls /dev/i2c-*
```

---

## NanoKVM

与 LicheeRV Nano 使用相同的 SG2002 SoC。GPIO 与 I2C 的访问沿用相同的引脚复用流程，请参考上文的 LicheeRV Nano 章节。

查看 NanoKVM 专用排针以确认可用的 I2C/SPI 线路：
- https://wiki.sipeed.com/hardware/en/kvm/NanoKVM/introduction.html

---

## 常见问题

### 找不到 devmem
`devmem` 工具可能不在默认镜像中。可选方案：
- 如果已安装 busybox，可使用 `busybox devmem`
- 从 Sipeed 软件源下载 devmem
- 从源码交叉编译（单个 C 文件）

### 总线编号动态变化
I2C 适配器编号可能随驱动加载顺序在每次启动后发生变化。请始终用 `i2c detect` 查询当前的总线分配，而不要硬编码总线编号。

### 权限
`/dev/i2c-*` 和 `/dev/spidev*` 通常需要 root 权限。可选方案：
- 以 root 身份运行 chagent
- 将用户加入 `i2c` 和 `spi` 用户组
- 创建 udev 规则：`SUBSYSTEM=="i2c-dev", MODE="0666"`
