package bridge

import "time"

type Diagnostics struct {
	Intake          string `json:"intake"`
	IntakePhase     string `json:"intake_phase"`
	IntakeAt        int64  `json:"intake_at"`
	IntakeRecovered int    `json:"intake_recovered"`
	LiveSync        string `json:"live_sync"`
	WatchEnabled    bool   `json:"watch_enabled"`
	ImportExisting  bool   `json:"import_existing"`
	ScanPhase       string `json:"scan_phase"`
	ScanMessage     string `json:"scan_message"`
	ScanAt          int64  `json:"scan_at"`
	MessagePhase    string `json:"message_phase"`
	Message         string `json:"message"`
	MessageAt       int64  `json:"message_at"`
}

func (b *Bridge) Diagnostics() Diagnostics {
	b.diagMu.Lock()
	defer b.diagMu.Unlock()
	d := b.diag
	d.WatchEnabled = b.cfg.Watch.Enabled
	d.ImportExisting = b.cfg.Watch.ImportExisting
	if d.ScanPhase == "" {
		if d.WatchEnabled {
			d.ScanPhase = "waiting"
			d.ScanMessage = "等待首次扫描"
		} else {
			d.ScanPhase = "disabled"
			d.ScanMessage = "自动发现未开启；请在机器人私聊发送 /new"
		}
	}
	return d
}
func (b *Bridge) scanDiagnostic(phase, message string) {
	b.diagMu.Lock()
	defer b.diagMu.Unlock()
	b.diag.ScanPhase = phase
	b.diag.ScanMessage = message
	b.diag.ScanAt = time.Now().Unix()
}
func (b *Bridge) messageDiagnostic(phase, message string) {
	b.diagMu.Lock()
	defer b.diagMu.Unlock()
	b.diag.MessagePhase = phase
	b.diag.Message = message
	b.diag.MessageAt = time.Now().Unix()
}
