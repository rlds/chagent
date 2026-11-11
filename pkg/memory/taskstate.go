package memory

import (
	"log"

	"github.com/rlds/chagent/pkg/taskstate"
)

// GetTaskState 返回存储在会话元数据中的持久化任务状态。
//
// 返回的状态是深拷贝，因此调用方无法通过共享切片修改存储。
// 会话不存在、遗留元数据文件缺少 task_state 字段，
// 或元数据文件无法读取时，均返回（零状态，false）；
// 最后一种情况会记录日志，因为它表示存储损坏，而不是“没有任务”。
func (s *JSONLStore) GetTaskState(sessionKey string) (taskstate.State, bool) {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		log.Printf("memory: get task state: %v", err)
		return taskstate.State{}, false
	}
	if meta.TaskState == nil || meta.TaskState.IsZero() {
		return taskstate.State{}, false
	}
	return meta.TaskState.Clone(), true
}

// SetTaskState 原子地替换会话的持久化任务状态。
// 零状态会清除先前存储的任何状态。元数据文件通过
// fileutil.WriteFileAtomic（临时文件 + fsync + 重命名）重写，
// 因此写入期间崩溃不会留下只写了一半的任务状态。
func (s *JSONLStore) SetTaskState(sessionKey string, state taskstate.State) error {
	l := s.sessionLock(sessionKey)
	l.Lock()
	defer l.Unlock()

	meta, err := s.readMeta(sessionKey)
	if err != nil {
		return err
	}
	if state.IsZero() {
		meta.TaskState = nil
	} else {
		if err := state.Validate(); err != nil {
			return err
		}
		cloned := state.Clone()
		meta.TaskState = &cloned
	}
	return s.writeMeta(sessionKey, meta)
}
