// Package channels 负责 IM 适配器及配置表单元数据注册。
package channels

import (
	"errors"
	"fmt"
	"im-agent/internal/config"
	"im-agent/internal/im"
	"sort"
)

type Factory func(config.Connection) (im.Adapter, error)
type CredentialField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
	Help     string `json:"help"`
}
type Descriptor struct {
	Type           string            `json:"type"`
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	UserHint       string            `json:"user_hint"`
	SupportsTopics bool              `json:"supports_topics"`
	Credentials    []CredentialField `json:"credentials"`
}
type registration struct {
	factory    Factory
	descriptor Descriptor
	validate   func(config.Connection) error
}
type Registry struct{ entries map[string]registration }

func NewRegistry() *Registry { return &Registry{entries: map[string]registration{}} }
func (r *Registry) Register(kind string, f Factory) error {
	return r.RegisterDescriptor(Descriptor{Type: kind, Name: kind}, f, nil)
}
func (r *Registry) RegisterDescriptor(d Descriptor, f Factory, v func(config.Connection) error) error {
	if d.Type == "" || f == nil {
		return errors.New("IM 类型和工厂不能为空")
	}
	if _, ok := r.entries[d.Type]; ok {
		return fmt.Errorf("重复注册 IM 类型 %s", d.Type)
	}
	r.entries[d.Type] = registration{f, d, v}
	return nil
}
func (r *Registry) Descriptors() []Descriptor {
	out := []Descriptor{}
	for _, e := range r.entries {
		out = append(out, e.descriptor)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}
func (r *Registry) Descriptor(kind string) (Descriptor, error) {
	e, ok := r.entries[kind]
	if !ok {
		return Descriptor{}, errors.New("未实现该 IM 渠道")
	}
	return e.descriptor, nil
}
func (r *Registry) Validate(c config.Connection) error {
	e, ok := r.entries[c.Type]
	if !ok {
		return errors.New("未注册该 IM 适配器")
	}
	if c.ConversationMode == "topic" && !e.descriptor.SupportsTopics {
		return errors.New("当前 IM 尚未实现话题模式")
	}
	if e.validate != nil {
		return e.validate(c)
	}
	return nil
}
func (r *Registry) Build(c config.Connection) (im.Adapter, error) {
	if e := r.Validate(c); e != nil {
		return nil, e
	}
	return r.entries[c.Type].factory(c)
}
