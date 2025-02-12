/*
Copyright 2023 The Alibaba Cloud Serverless Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scaler

import (
	"container/list"
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AliyunContainerService/scaler/go/pkg/config"
	model2 "github.com/AliyunContainerService/scaler/go/pkg/model"
	platform_client2 "github.com/AliyunContainerService/scaler/go/pkg/platform_client"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/AliyunContainerService/scaler/proto"
	"github.com/google/uuid"
)

type Simple struct {
	config         *config.Config
	metaData       *model2.Meta
	platformClient platform_client2.Client
	mu             sync.Mutex
	wg             sync.WaitGroup
	// instances内存映射表,key是实例id
	instances map[string]*model2.Instance
	// instances空闲队列
	idleInstance    *list.List
	longPollingMu   sync.Mutex
	longPollingList *list.List
	// 正在创建的实例数
	creatingNum      int64
	runtimeStatus    *RuntimeStatus
	creatingDuration int64
}

func New(metaData *model2.Meta, config *config.Config) Scaler {
	client, err := platform_client2.New(config.ClientAddr)
	if err != nil {
		log.Fatalf("client init with error: %s", err.Error())
	}
	scheduler := &Simple{
		config:          config,
		metaData:        metaData,
		platformClient:  client,
		mu:              sync.Mutex{},
		wg:              sync.WaitGroup{},
		instances:       make(map[string]*model2.Instance),
		idleInstance:    list.New(),
		longPollingMu:   sync.Mutex{},
		longPollingList: list.New(),
		creatingNum:     0,
		runtimeStatus:   NewRuntimeStatus(),
	}
	log.Printf("New scaler for app: %s is created", metaData.Key)
	// 回收pod
	scheduler.wg.Add(1)
	go func() {
		defer scheduler.wg.Done()
		scheduler.gcLoop()
		log.Printf("gc loop for app: %s is stoped", metaData.Key)
	}()

	return scheduler
}

// 通知等待的请求,有空闲的instance
func (s *Simple) notifyRequest(instance *model2.Instance) {
	s.longPollingMu.Lock()
	// 如果有等待的长轮询请求
	if element := s.longPollingList.Front(); element != nil {
		// 有长轮询请求
		log.Printf("notify long polling request, instance: %s", instance.Id)
		// 获取请求的channel
		longPollingChan := element.Value.(chan *model2.Instance)
		// 从队列中删除
		s.longPollingList.Remove(element)
		// 发送实例通知
		longPollingChan <- instance
		s.longPollingMu.Unlock()
	} else {
		// 没有等待请求，将释放的instance加入到空闲资源池
		log.Printf("add to idleInstance, instance: %s", instance.Id)
		s.longPollingMu.Unlock()
		instance.Busy = false
		instance.LastIdleTime = time.Now()
		s.mu.Lock()
		s.idleInstance.PushFront(instance)
		s.mu.Unlock()
	}
}

// Assign 处理分配实例请求
func (s *Simple) Assign(ctx context.Context, request *pb.AssignRequest) (*pb.AssignReply, error) {
	go s.runtimeStatus.AssignStart(time.Now())
	log.Printf("Assign, request id: %s", request.RequestId)
	defer func() {
		go s.runtimeStatus.AssignReturn(request.RequestId)
	}()
	// 记录处理开始时间
	start := time.Now()
	// 有空闲资源
	s.mu.Lock()
	if element := s.idleInstance.Front(); element != nil {
		instance := element.Value.(*model2.Instance)
		// 设置实例为忙碌
		instance.Busy = true
		// 从空闲队列中移除
		s.idleInstance.Remove(element)
		s.mu.Unlock()
		log.Printf("Assign idleInstance, request id: %s, instance %s, cost time = %s", request.RequestId, instance.Id, time.Since(start))
		return &pb.AssignReply{
			Status: pb.Status_Ok,
			Assigment: &pb.Assignment{
				RequestId:  request.RequestId,
				MetaKey:    instance.Meta.Key,
				InstanceId: instance.Id,
			},
			ErrorMessage: nil,
		}, nil
	}
	s.mu.Unlock()

	// 无空闲资源
	longPollingChan := make(chan *model2.Instance, 1)
	s.longPollingMu.Lock()
	s.longPollingList.PushBack(longPollingChan)

	// create instance limit
	// 如果当前创建数没有达到限制,创建新实例
	if s.longPollingList.Len() > int(atomic.LoadInt64(&s.creatingNum)) {
		go func() {
			s.createInstance(request.MetaData, request.RequestId)
		}()
	}
	s.longPollingMu.Unlock()

	select {
	case <-ctx.Done():
		log.Printf("assign timeout request id: %s", request.RequestId)
		return nil, ctx.Err()
	case instance := <-longPollingChan:
		instance.Busy = true
		log.Printf("Assign longPolling, request id: %s, instance %s, cost time: %s", request.RequestId, instance.Id, time.Since(start))
		return &pb.AssignReply{
			Status: pb.Status_Ok,
			Assigment: &pb.Assignment{
				RequestId:  request.RequestId,
				MetaKey:    instance.Meta.Key,
				InstanceId: instance.Id,
			},
			ErrorMessage: nil,
		}, nil
	}
}

func (s *Simple) Idle(ctx context.Context, request *pb.IdleRequest) (*pb.IdleReply, error) {
	go func() {
		s.runtimeStatus.IdleStart(request.Assigment.RequestId)
	}()
	if request.Assigment == nil {
		return nil, status.Errorf(codes.InvalidArgument, "assignment is nil")
	}
	reply := &pb.IdleReply{
		Status:       pb.Status_Ok,
		ErrorMessage: nil,
	}
	start := time.Now()
	instanceId := request.Assigment.InstanceId
	defer func() {
		log.Printf("Idle, request id: %s, instance: %s, cost %dus", request.Assigment.RequestId, instanceId, time.Since(start).Microseconds())
	}()
	//log.Printf("Idle, request id: %s", request.Assigment.RequestId)
	needDestroy := false
	slotId := ""
	if request.Result != nil && request.Result.NeedDestroy != nil && *request.Result.NeedDestroy {
		needDestroy = true
	}
	defer func() {
		if needDestroy {
			s.deleteSlot(ctx, request.Assigment.RequestId, slotId, instanceId, request.Assigment.MetaKey, "bad instance")
		}
	}()
	log.Printf("Idle, request id: %s", request.Assigment.RequestId)
	s.mu.Lock()
	defer s.mu.Unlock()
	if instance := s.instances[instanceId]; instance != nil {
		slotId = instance.Slot.Id
		if needDestroy {
			log.Printf("request id %s, instance %s need be destroy", request.Assigment.RequestId, instanceId)
			return reply, nil
		}

		if !instance.Busy {
			log.Printf("request id %s, instance %s already freed", request.Assigment.RequestId, instanceId)
			return reply, nil
		}

		go func() {
			log.Printf("Idle notify request, instance: %s", instance.Id)
			s.notifyRequest(instance)
		}()

	} else {
		return nil, status.Errorf(codes.NotFound, fmt.Sprintf("request id %s, instance %s not found", request.Assigment.RequestId, instanceId))
	}
	return &pb.IdleReply{
		Status:       pb.Status_Ok,
		ErrorMessage: nil,
	}, nil
}

func (s *Simple) deleteSlot(ctx context.Context, requestId, slotId, instanceId, metaKey, reason string) {
	log.Printf("start delete Instance %s (Slot: %s) of app: %s", instanceId, slotId, metaKey)
	if err := s.platformClient.DestroySLot(ctx, requestId, slotId, reason); err != nil {
		log.Printf("delete Instance %s (Slot: %s) of app: %s failed with: %s", instanceId, slotId, metaKey, err.Error())
	}
}

// 周期回收
func (s *Simple) gcLoop() {
	log.Printf("gc loop for app: %s is started", s.metaData.Key)
	ticker := time.NewTicker(s.config.GcInterval)
	for range ticker.C {
		for {
			s.mu.Lock()
			if element := s.idleInstance.Back(); element != nil {
				instance := element.Value.(*model2.Instance)
				idleDuration := time.Since(instance.LastIdleTime)
				if idleDuration > s.config.IdleDurationBeforeGC {
					//need GC
					s.idleInstance.Remove(element)
					// 从map删除
					delete(s.instances, instance.Id)
					s.mu.Unlock()
					// 回收实例
					go func() {
						reason := fmt.Sprintf("Idle duration: %fs, excceed configured duration: %fs", idleDuration.Seconds(), s.config.IdleDurationBeforeGC.Seconds())
						ctx := context.Background()
						ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
						defer cancel()
						s.deleteSlot(ctx, uuid.NewString(), instance.Slot.Id, instance.Id, instance.Meta.Key, reason)
					}()
					continue
				}
			}
			s.mu.Unlock()
			break
		}
	}
}

func (s *Simple) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		TotalInstance:     len(s.instances),
		TotalIdleInstance: s.idleInstance.Len(),
	}
}

func (s *Simple) createInstance(requestMeta *pb.Meta, requestId string) {
	creatingTime := time.Now()
	// 将creating数量+1
	atomic.AddInt64(&s.creatingNum, 1)
	defer atomic.AddInt64(&s.creatingNum, -1)
	//Create new Instance
	instanceId := uuid.New().String()
	resourceConfig := model2.SlotResourceConfig{
		ResourceConfig: pb.ResourceConfig{
			MemoryInMegabytes: requestMeta.MemoryInMb,
		},
	}

	slot, err := s.platformClient.CreateSlot(context.Background(), requestId, &resourceConfig)
	if err != nil {
		log.Printf("create slot failed with: %s", err.Error())
		return
	}

	meta := &model2.Meta{
		Meta: pb.Meta{
			Key:           requestMeta.Key,
			Runtime:       requestMeta.Runtime,
			TimeoutInSecs: requestMeta.TimeoutInSecs,
		},
	}
	instance, err := s.platformClient.Init(context.Background(), requestId, instanceId, slot, meta)
	if err != nil {
		log.Printf("create instance failed with: %s", err.Error())
		return
	}

	s.mu.Lock()
	s.instances[instance.Id] = instance
	s.mu.Unlock()

	//notify
	go func() {
		log.Printf("createInstance notify request, instance: %s", instance.Id)
		s.notifyRequest(instance)
	}()
	go atomic.CompareAndSwapInt64(&s.creatingDuration, 0, int64(time.Since(creatingTime)))
	log.Printf("request id: %s, instance %s for app %s is created, init latency: %dms", requestId, instance.Id, instance.Meta.Key, instance.InitDurationInMs)
}

func (s *Simple) CheckLive() bool {
	// 超过45秒没有消息的时候，返回false
	return true
}

func (s *Simple) Clear(rate float64) {
	// 按照比例释放idleInstance
}
