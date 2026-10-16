package codex

// message/assistant 是公开消息；部分提供方不填写 phase，不能等同于无输出。
// 缺省 phase 且条目 completed 只表示公开候选；是否为最终回复由同轮次完成正文核验。
func completedPublicReply(phase, status string) bool {
	return phase == "final_answer" || phase == "" && status == "completed"
}
func publicAssistantPhase(phase string) bool {
	return phase == "" || phase == "commentary" || phase == "final_answer"
}
