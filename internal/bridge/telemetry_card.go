package bridge

import "time"

func telemetryTime(value int64) string {
	if value <= 0 {
		return "未采集"
	}
	return time.UnixMilli(value).Format("01-02 15:04:05")
}
