package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
)

// sessionTarget 用 rendezvous hashing（最高随机权重）确定同组首选。
// 分数只依赖会话、档次和 Target，不依赖 cursor、候选顺序或进程内状态。
// 配置相同时，重启和其他网关实例也能算出同一个首选；容量仍按进程管理。
// JSON 数组为各字符串提供明确边界，避免简单拼接产生歧义。
func sessionTarget(group []Target, r Request) Target {
	if r.Session == "" {
		return Target{}
	}
	var preferred Target
	var highest [sha256.Size]byte
	for i, target := range group {
		key, _ := json.Marshal([]string{r.Session, r.Tier, target.Provider, target.Model})
		score := sha256.Sum256(key)
		comparison := bytes.Compare(score[:], highest[:])
		// 分数极少相同时仍用稳定的 Target 顺序决胜。
		if i == 0 || comparison > 0 || (comparison == 0 && (target.Provider < preferred.Provider || (target.Provider == preferred.Provider && target.Model < preferred.Model))) {
			preferred, highest = target, score
		}
	}
	return preferred
}
