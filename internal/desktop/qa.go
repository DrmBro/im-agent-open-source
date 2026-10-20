package desktop

import (
	"context"
	"errors"
	"time"

	"im-agent/internal/qa"
	"im-agent/internal/service"
	"im-agent/internal/store"
)

func (a *API) GetQAState() (store.QAView, error) {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return store.QAView{}, e
	}
	view, e := a.app.store.Read()
	parent := a.app.ctx
	a.app.mu.Unlock()
	if e != nil || parent == nil {
		return store.QAView{}, errors.New("问答配置暂不可读取")
	}
	ctx, stop := context.WithTimeout(parent, 5*time.Second)
	defer stop()
	return store.QAStateSnapshot(ctx, view.Config.StateDir)
}

// QADocumentView 仅在用户主动查看时返回本机文档正文。
type QADocumentView struct {
	Artifact store.QAArtifact `json:"artifact"`
	Markdown string           `json:"markdown"`
}

func (a *API) GetQADocument(id string) (QADocumentView, error) {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return QADocumentView{}, e
	}
	view, e := a.app.store.Read()
	parent := a.app.ctx
	a.app.mu.Unlock()
	if e != nil || parent == nil {
		return QADocumentView{}, errors.New("问答配置暂不可读取")
	}
	ctx, stop := context.WithTimeout(parent, 5*time.Second)
	defer stop()
	artifact, e := store.QAArtifactSnapshot(ctx, view.Config.StateDir, id)
	if e != nil {
		return QADocumentView{}, e
	}
	markdown, e := qa.ReadArtifact(view.Config.StateDir, artifact)
	if e != nil {
		return QADocumentView{}, e
	}
	return QADocumentView{Artifact: artifact, Markdown: markdown}, nil
}

func (a *API) ResolveQARequest(request qa.Resolution) error {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return e
	}
	client := a.app.controller
	a.app.mu.Unlock()
	return client.ResolveQARequest(request)
}
func (a *API) ResolveQADelivery(request service.QADeliveryResolution) error {
	a.app.mu.Lock()
	if e := a.app.ready(); e != nil {
		a.app.mu.Unlock()
		return e
	}
	client := a.app.controller
	a.app.mu.Unlock()
	return client.ResolveQADelivery(request)
}
