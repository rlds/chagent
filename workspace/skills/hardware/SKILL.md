---
name: hardware
description: 读取并控制 Sipeed 板卡（LicheeRV Nano、MaixCAM、NanoKVM）上的 I2C 与 SPI 外设。
homepage: https://wiki.sipeed.com/hardware/en/lichee/RV_Nano/1_intro.html
metadata: {"nanobot":{"emoji":"🔧","requires":{"tools":["i2c","spi"]}}}
---

# 硬件（I2C / SPI）

使用 `i2c` 和 `spi` 工具与板卡上连接的传感器、显示屏及其他外设交互。

## 快速开始

```
# 1. 查找可用总线
i2c detect

# 2. 扫描已连接的设备
i2c scan  (bus: "1")

# 3. 从传感器读取数据（例如 AHT20 温湿度传感器）
i2c read  (bus: "1", address: 0x38, register: 0xAC, length: 6)

# 4. SPI 设备
spi list
spi read  (device: "2.0", length: 4)
```

## 开始之前 —— Pinmux 配置

在 Sipeed 板卡上，大多数 I2C/SPI 引脚与 WiFi 复用。使用前必须先配置 pinmux。

板卡专用命令见 `references/board-pinout.md`。

**通用步骤：**
1. 若使用复用引脚，先停止 WiFi：`/etc/init.d/S30wifi stop`
2. 加载 i2c-dev 模块：`modprobe i2c-dev`
3. 使用 `devmem` 配置 pinmux（具体因板卡而异）
4. 用 `i2c detect` 和 `i2c scan` 验证

## 安全性

- **写操作**需要 `confirm: true` —— 请务必先与用户确认
- I2C 地址会校验为 7 位范围（0x03-0x77）
- SPI 模式会校验（仅限 0-3）
- 单次传输上限：256 字节（I2C）、4096 字节（SPI）

## 常用设备

寄存器映射与热门传感器的用法见 `references/common-devices.md`：
AHT20、BME280、SSD1306 OLED、MPU6050 IMU、DS3231 RTC、INA219 功率监测器、PCA9685 PWM 等。

## 故障排查

| 问题 | 解决方案 |
|---------|----------|
| 未找到 I2C 总线 | 执行 `modprobe i2c-dev` 并检查设备树 |
| 权限不足 | 以 root 运行，或将用户加入 `i2c` 用户组 |
| 扫描不到设备 | 检查接线、上拉电阻（典型 4.7k）和 pinmux |
| 总线编号发生变化 | I2C 适配器编号在不同启动之间可能变化；用 `i2c detect` 查询当前分配 |
| WiFi 停止工作 | I2C-1/SPI-2 与 WiFi SDIO 复用引脚，无法同时使用 |
| 找不到 `devmem` | 单独下载，或改用 `busybox devmem` |
| SPI 传输返回全 0 | 检查 MISO 接线和设备供电 |
| SPI 传输返回全 0xFF | 设备无响应；检查 CS 引脚和时钟极性（模式） |
