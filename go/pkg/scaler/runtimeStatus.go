package scaler

import (
	"container/list"
	"github.com/AliyunContainerService/scaler/go/pkg/config"
	"sync"
	"time"
)

type RuntimeStatus struct {
	requestDuration   map[string]time.Time
	requestDurationMu sync.Mutex
	requestCostTime   time.Duration
	rctRate           float64
	requestInstance   *list.List
	requestInstanceMu sync.Mutex
	maxRequestNum     int64
}

func NewRuntimeStatus() *RuntimeStatus {
	r := &RuntimeStatus{
		requestDuration:   make(map[string]time.Time),
		requestDurationMu: sync.Mutex{},
		rctRate:           config.DefaultConfig.RctRate,
		requestInstanceMu: sync.Mutex{},
		requestInstance:   list.New(),
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

func (r *RuntimeStatus) AssignStart(timeStamp time.Time) {
	requestCostTime := r.GetRequestCostTime()
	r.requestInstanceMu.Lock()
	defer r.requestInstanceMu.Unlock()
	r.requestInstance.PushBack(timeStamp)
	// 遍历request队列，timeStamp>requestCostTime则删除
	for element := r.requestInstance.Front(); element.Value.(time.Time) != timeStamp; element = element.Next() {
		elemTimeStamp := element.Value.(time.Time)
		if time.Since(elemTimeStamp) > requestCostTime {
			r.requestInstance.Remove(element)
		}
	}
	//记录当前请求数量
	requestNum := r.requestInstance.Len()
	// 更新最大并发请求数量
	if int64(requestNum) > r.maxRequestNum {
		r.maxRequestNum = int64(requestNum)
	}
}

func (r *RuntimeStatus) getMaxRequestBNum() int64 {
	return r.maxRequestNum
}

func (r *RuntimeStatus) getCurrentRequestBNum() int64 {
	requestCostTime := r.GetRequestCostTime()
	r.requestInstanceMu.Lock()
	defer r.requestInstanceMu.Unlock()
	// 遍历request队列，timeStamp>requestCostTime则删除
	for element := r.requestInstance.Front(); element != nil; element = element.Next() {
		elemTimeStamp := element.Value.(time.Time)
		if time.Since(elemTimeStamp) > requestCostTime {
			r.requestInstance.Remove(element)
		}
	}
	//记录当前请求数量
	requestNum := int64(r.requestInstance.Len())
	// 更新最大并发请求数量
	if requestNum > r.maxRequestNum {
		r.maxRequestNum = requestNum
	}
	return requestNum
}
