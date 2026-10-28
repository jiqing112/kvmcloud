package task

import (
	"context"
	"fmt"
)

// Bus 是对消息队列的最小抽象：把 Envelope 投到 Kind 对应的主题。
//
// 本包不直接 import nats.go，生产实现（JetStream + Nats-Msg-Id 幂等去重 + 发布超时）
// 在 docs/api/design.md 中给出接线代码；单测用 BusFunc 打桩。
type Bus interface {
	Publish(ctx context.Context, env *Envelope) error
}

// BusFunc 便于测试与简单实现。
type BusFunc func(ctx context.Context, env *Envelope) error

// Publish 实现 Bus。
func (f BusFunc) Publish(ctx context.Context, env *Envelope) error { return f(ctx, env) }

// Publisher 负责序列化并按 Kind 路由主题。业务层只依赖 Publisher，
// 不感知主题命名与编码细节。
type Publisher struct {
	bus Bus
}

// NewPublisher 构造。
func NewPublisher(bus Bus) *Publisher { return &Publisher{bus: bus} }

// Publish 把任务封装为信封并投递到 kvmcloud.tasks.<kind>。
// 失败时返回错误，由调用方决定是否将任务标记 failed（详见 service 层补偿）。
func (p *Publisher) Publish(ctx context.Context, t *Task) error {
	if t == nil {
		return fmt.Errorf("task: nil task")
	}
	if err := p.bus.Publish(ctx, t.Envelope()); err != nil {
		return fmt.Errorf("task: publish %s: %w", Subject(t.Kind), err)
	}
	return nil
}
