package bridge

import (
	"fmt"
	"strings"
	"time"

	"im-agent/internal/telemetry"
)

func telemetryTime(value int64) string {
	if value <= 0 {
		return "未采集"
	}
	return time.UnixMilli(value).Format("01-02 15:04:05")
}

func telemetryCount(value *int64) string {
	if value == nil || *value < 0 {
		return "未采集"
	}
	return fmt.Sprintf("%d", *value)
}

func requestTelemetryText(report *telemetry.Report) string {
	if report == nil {
		return "\n\n运行元数据：未采集（等待确切请求关联或数据源暂不可用）。"
	}
	snapshot := report.Snapshot
	var text strings.Builder
	text.WriteString("\n\n运行元数据（会话日志观察值）")
	model := snapshot.Model
	if model == "" {
		model = "未采集"
	}
	fmt.Fprintf(&text, "\n模型：%s\n执行开始：%s", model, telemetryTime(snapshot.StartedAt))
	fmt.Fprintf(&text, "\n首次可确认输出：%s\n最新可确认输出：%s", telemetryTime(snapshot.FirstOutputAt), telemetryTime(snapshot.LastOutputAt))
	text.WriteString("\n真实首 token 延迟（TTFT）：未采集，日志时间不等于流式首 token")
	activities := map[string]string{"started": "请求已关联", "output": "模型文本", "tool": "工具活动", "usage": "用量上报", "task_complete": "轮次完成", "turn_aborted": "轮次中断"}
	fmt.Fprintf(&text, "\n最近活动：%s", telemetryTime(snapshot.LastActivityAt))
	if label := activities[snapshot.Activity]; label != "" {
		fmt.Fprintf(&text, " · %s", label)
	}
	if usage := snapshot.TurnUsage; usage.Valid() {
		fmt.Fprintf(&text, "\n本轮累计 token：输入 %s / 输出 %s", telemetryCount(usage.Input), telemetryCount(usage.Output))
		fmt.Fprintf(&text, "\n其中缓存命中输入：%s / 推理输出：%s", telemetryCount(usage.CachedInput), telemetryCount(usage.Reasoning))
	} else {
		text.WriteString("\n本轮累计 token：未采集")
	}
	if usage := snapshot.LastCall; usage.Valid() {
		fmt.Fprintf(&text, "\n最近调用 token：输入 %s / 输出 %s", telemetryCount(usage.Input), telemetryCount(usage.Output))
		if snapshot.ContextWindow != nil && *snapshot.ContextWindow > 0 {
			fmt.Fprintf(&text, "\n输入占窗口：%s / %s（%.1f%%）", telemetryCount(usage.Input), telemetryCount(snapshot.ContextWindow), float64(*usage.Input)/float64(*snapshot.ContextWindow)*100)
			text.WriteString("\n占比不扣缓存，不代表精确剩余可用上下文。")
		} else {
			text.WriteString("\n上下文窗口：未采集")
		}
		fmt.Fprintf(&text, "\n用量截至：%s", telemetryTime(snapshot.UsageAt))
	} else {
		text.WriteString("\n最近调用用量及上下文窗口：未采集")
	}
	if snapshot.OutputConflict {
		text.WriteString("\n部分输出的轮次身份未通过核对，未用于首次/最新输出时间。")
	}
	if snapshot.CompletionOutputVerified {
		text.WriteString("\n最终输出时间已通过目标轮次完成事件与原回复逐字一致性核验。")
	}
	switch snapshot.Issue {
	case "identity_conflict":
		text.WriteString("\n请求来源存在歧义，已停止本请求统计，未猜测归属。")
	case "invalid_usage":
		text.WriteString("\n最近用量格式无效，保留此前可用快照，不以零代替。")
	case "read_failed":
		text.WriteString("\n元数据读取暂不可用，显示的是此前快照，后续降频核对。")
	case "state_invalid":
		text.WriteString("\n元数据进度不可确认，未自动回退重算。")
	}
	fmt.Fprintf(&text, "\n数据观察时间：%s", telemetryTime(snapshot.ObservedAt))
	return text.String()
}
