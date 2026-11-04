// Package ctxlock provides cancellation while waiting for serialized dispatch.
package ctxlock

import (
	"context"
	"sync"
)

type Mutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *Mutex) LockContext(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	m.once.Do(func() { m.token = make(chan struct{}, 1); m.token <- struct{}{} })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		return nil
	}
}
func (m *Mutex) Lock()   { _ = m.LockContext(context.Background()) }
func (m *Mutex) Unlock() { m.token <- struct{}{} }
