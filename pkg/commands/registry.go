package commands

type Registry struct {
	defs  []Definition
	index map[string]int
}

// NewRegistry 存储规范命令集合，供分发流程与可选的平台注册适配器
// 共同使用。
func NewRegistry(defs []Definition) *Registry {
	stored := make([]Definition, len(defs))
	copy(stored, defs)

	index := make(map[string]int, len(stored)*2)
	for i, def := range stored {
		registerCommandName(index, def.Name, i)
		for _, alias := range def.Aliases {
			registerCommandName(index, alias, i)
		}
	}

	return &Registry{defs: stored, index: index}
}

// Definitions 返回所有已注册的命令定义。
// 命令可用性是全局的，不再按 channel 划分。
func (r *Registry) Definitions() []Definition {
	out := make([]Definition, len(r.defs))
	copy(out, r.defs)
	return out
}

// Lookup 按规范化后的命令名或别名查找命令定义。
func (r *Registry) Lookup(name string) (Definition, bool) {
	key := normalizeCommandName(name)
	if key == "" {
		return Definition{}, false
	}
	idx, ok := r.index[key]
	if !ok {
		return Definition{}, false
	}
	return r.defs[idx], true
}

func registerCommandName(index map[string]int, name string, defIndex int) {
	key := normalizeCommandName(name)
	if key == "" {
		return
	}
	if _, exists := index[key]; exists {
		return
	}
	index[key] = defIndex
}
