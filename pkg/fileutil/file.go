// Chagent - 超轻量级个人 AI 代理
// 灵感来自并基于 nanobot：https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

// Package fileutil 提供文件操作工具。
package fileutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic 使用临时文件 + 重命名模式，以原子方式将数据写入文件。
//
// 这保证目标文件要么：
// - 被新数据完整写入
// - 保持未变（如果在重命名前的任何步骤失败）
//
// 该函数：
// 1. 在同一目录下创建一个临时文件（原文件不受影响）
// 2. 将数据写入临时文件
// 3. 将数据同步到磁盘（对 SD 卡/闪存存储至关重要）
// 4. 设置文件权限
// 5. 同步目录元数据（确保重命名持久化）
// 6. 以原子方式将临时文件重命名为目标路径
//
// 安全保证：
// - 在成功重命名之前，原文件绝不会被修改
// - 出错时临时文件总会被清理
// - 数据在重命名前被刷写到物理存储
// - 目录项被同步，以防止产生孤儿 inode
//
// 参数：
//   - path：目标文件路径
//   - data：要写入的数据
//   - perm：文件权限模式（例如 0o600 表示安全，0o644 表示可读）
//
// 返回：
//   - 任何步骤失败时返回错误，成功时返回 nil
//
// 示例：
//
//	// 安全配置文件（仅所有者读写）
//	err := utils.WriteFileAtomic("config.json", data, 0o600)
//
//	// 公开可读文件
//	err := utils.WriteFileAtomic("public.txt", data, 0o644)
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// 在同一目录下创建一个唯一的临时文件（原子重命名需要
	// 位于同一文件系统）。即使多个 goroutine 并发写入，
	// os.CreateTemp 也能保证唯一性；隐藏前缀能让工具链满意。
	tmpFile, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	if err := tmpFile.Chmod(perm); err != nil {
		tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
		return fmt.Errorf("failed to chmod temp file: %w", err)
	}

	tmpPath := tmpFile.Name()
	cleanup := true

	defer func() {
		if cleanup {
			tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// 将数据写入临时文件
	// 注意：此时原文件未被改动
	if _, err := tmpFile.Write(data); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// 关键：在任何其他操作之前强制同步到存储介质。
	// 这确保数据被物理写入磁盘，而不只是停留在缓存中。
	// 对 SD 卡、eMMC 以及边缘设备上的其他闪存存储至关重要。
	if err := tmpFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	// 在关闭前设置文件权限
	if err := tmpFile.Chmod(perm); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	// 在重命名前关闭文件（Windows 上必需）
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	// 原子重命名：临时文件成为目标文件
	// 在 POSIX 上：rename() 是原子的
	// 在 Windows 上：Rename() 对文件是原子的
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	// 同步目录以确保重命名持久化
	// 这可防止重命名后的文件在崩溃后消失
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}

	// 成功：跳过清理（文件已被重命名，没有临时文件需要删除）
	cleanup = false
	return nil
}

func CopyFile(src, dst string, perm os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return WriteFileAtomic(dst, data, perm)
}
