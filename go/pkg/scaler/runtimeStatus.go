package scaler

import (
	"sync"
	"time"
)

type RuntimeStatus struct {
	requestDuration   map[string]time.Time
	requestDurationMu sync.Mutex
	requestCostTime   time.Duration
	rctRate           float64
}

func NewRuntimeStatus() *RuntimeStatus {
	r := &RuntimeStatus{
		requestDuration:   make(map[string]time.Time),
		requestDurationMu: sync.Mutex{},
		rctRate:           0.9,
	}
	return r
}

func (r *RuntimeStatus) AssignReturn(requestId string) {
	r.requestDurationMu.Lock()
	defer r.requestDurationMu.Unlock()
	// 记录处理开始时间
	r.requestDuration[requestId] = time.Now()
}

func (r *RuntimeStatus) IdleStart(requestId string) {
	r.requestDurationMu.Lock()
	defer r.requestDurationMu.Unlock()
	// Duration
	duration := time.Since(r.requestDuration[requestId])
	if r.requestCostTime == 0 {
		r.requestCostTime = duration
	} else {
		// 旧duration * rate + 新duration * (1 - rate)
		r.requestCostTime = time.Duration(r.rctRate*float64(r.requestCostTime) + (1-r.rctRate)*float64(duration))
	}
}

func (r *RuntimeStatus) GetRequestCostTime() time.Duration {
	r.requestDurationMu.Lock()
	defer r.requestDurationMu.Unlock()
	return r.requestCostTime
}
