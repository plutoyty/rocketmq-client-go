/*
Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package producer

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/apache/rocketmq-client-go/internal"
	"github.com/apache/rocketmq-client-go/internal/remote"
	"github.com/apache/rocketmq-client-go/internal/utils"
	"github.com/apache/rocketmq-client-go/primitive"
	"github.com/apache/rocketmq-client-go/rlog"
)

var (
	ErrTopicEmpty   = errors.New("topic is nil")
	ErrMessageEmpty = errors.New("message is nil")
	ErrNotRunning   = errors.New("producer not started")
)

type defaultProducer struct {
	group       string
	client      internal.RMQClient
	state       internal.ServiceState
	options     producerOptions
	publishInfo sync.Map
	callbackCh  chan interface{}

	interceptor primitive.Interceptor
}

func NewDefaultProducer(opts ...Option) (*defaultProducer, error) {
	defaultOpts := defaultProducerOptions()
	for _, apply := range opts {
		apply(&defaultOpts)
	}
	srvs, err := internal.NewNamesrv(defaultOpts.NameServerAddrs...)
	if err != nil {
		return nil, errors.Wrap(err, "new Namesrv failed.")
	}
	if !defaultOpts.Credentials.IsEmpty() {
		srvs.SetCredentials(defaultOpts.Credentials)
	}
	internal.RegisterNamsrv(srvs)

	producer := &defaultProducer{
		group:      defaultOpts.GroupName,
		callbackCh: make(chan interface{}),
		options:    defaultOpts,
	}
	producer.client = internal.GetOrNewRocketMQClient(defaultOpts.ClientOptions, producer.callbackCh)

	producer.interceptor = primitive.ChainInterceptors(producer.options.Interceptors...)

	return producer, nil
}

func (p *defaultProducer) Start() error {
	p.state = internal.StateRunning
	p.client.RegisterProducer(p.group, p)
	p.client.Start()
	return nil
}

func (p *defaultProducer) Shutdown() error {
	p.state = internal.StateShutdown
	p.client.Shutdown()
	return nil
}

func (p *defaultProducer) checkMsg(msg *primitive.Message) error {
	if p.state != internal.StateRunning {
		return ErrNotRunning
	}

	if msg == nil {
		return errors.New("message is nil")
	}

	if msg.Topic == "" {
		return errors.New("topic is nil")
	}
	return nil
}

func (p *defaultProducer) SendSync(ctx context.Context, msg *primitive.Message) (*primitive.SendResult, error) {
	if err := p.checkMsg(msg); err != nil {
		return nil, err
	}

	resp := new(primitive.SendResult)
	if p.interceptor != nil {
		primitive.WithMethod(ctx, primitive.SendSync)
		producerCtx := &primitive.ProducerCtx{
			ProducerGroup:     p.group,
			CommunicationMode: primitive.SendSync,
			BornHost:          utils.LocalIP,
			Message:           *msg,
			SendResult:        resp,
		}
		ctx = primitive.WithProducerCtx(ctx, producerCtx)

		err := p.interceptor(ctx, msg, resp, func(ctx context.Context, req, reply interface{}) error {
			var err error
			realReq := req.(*primitive.Message)
			realReply := reply.(*primitive.SendResult)
			err = p.sendSync(ctx, realReq, realReply)
			return err
		})
		return resp, err
	}

	err := p.sendSync(ctx, msg, resp)
	return resp, err
}

func (p *defaultProducer) sendSync(ctx context.Context, msg *primitive.Message, resp *primitive.SendResult) error {

	retryTime := 1 + p.options.RetryTimes

	var (
		err error
	)

	if p.options.Namespace != "" {
		msg.Topic = p.options.Namespace + "%" + msg.Topic
	}

	var producerCtx *primitive.ProducerCtx
	for retryCount := 0; retryCount < retryTime; retryCount++ {
		mq := p.selectMessageQueue(msg)
		if mq == nil {
			err = fmt.Errorf("the topic=%s route info not found", msg.Topic)
			continue
		}

		addr := internal.FindBrokerAddrByName(mq.BrokerName)
		if addr == "" {
			return fmt.Errorf("topic=%s route info not found", mq.Topic)
		}

		if p.interceptor != nil {
			producerCtx = primitive.GetProducerCtx(ctx)
			producerCtx.BrokerAddr = addr
			producerCtx.MQ = *mq
		}

		res, _err := p.client.InvokeSync(addr, p.buildSendRequest(mq, msg), 3*time.Second)
		if _err != nil {
			err = _err
			continue
		}
		return p.client.ProcessSendResponse(mq.BrokerName, res, resp, msg)
	}
	return err
}

func (p *defaultProducer) SendAsync(ctx context.Context, f func(context.Context, *primitive.SendResult, error), msg *primitive.Message) error {
	if err := p.checkMsg(msg); err != nil {
		return err
	}

	if p.interceptor != nil {
		primitive.WithMethod(ctx, primitive.SendAsync)

		return p.interceptor(ctx, msg, nil, func(ctx context.Context, req, reply interface{}) error {
			return p.sendAsync(ctx, msg, f)
		})
	}
	return p.sendAsync(ctx, msg, f)
}

func (p *defaultProducer) sendAsync(ctx context.Context, msg *primitive.Message, h func(context.Context, *primitive.SendResult, error)) error {
	if p.options.Namespace != "" {
		msg.Topic = p.options.Namespace + "%" + msg.Topic
	}
	mq := p.selectMessageQueue(msg)
	if mq == nil {
		return errors.Errorf("the topic=%s route info not found", msg.Topic)
	}

	addr := internal.FindBrokerAddrByName(mq.BrokerName)
	if addr == "" {
		return errors.Errorf("topic=%s route info not found", mq.Topic)
	}

	return p.client.InvokeAsync(addr, p.buildSendRequest(mq, msg), 3*time.Second, func(command *remote.RemotingCommand, err error) {
		resp := new(primitive.SendResult)
		if err != nil {
			h(ctx, nil, err)
		} else {
			p.client.ProcessSendResponse(mq.BrokerName, command, resp, msg)
			h(ctx, resp, nil)
		}
	})
}

func (p *defaultProducer) SendOneWay(ctx context.Context, msg *primitive.Message) error {
	if err := p.checkMsg(msg); err != nil {
		return err
	}

	if p.interceptor != nil {
		primitive.WithMethod(ctx, primitive.SendOneway)
		return p.interceptor(ctx, msg, nil, func(ctx context.Context, req, reply interface{}) error {
			return p.SendOneWay(ctx, msg)
		})
	}

	return p.sendOneWay(ctx, msg)
}

func (p *defaultProducer) sendOneWay(ctx context.Context, msg *primitive.Message) error {
	retryTime := 1 + p.options.RetryTimes

	if p.options.Namespace != "" {
		msg.Topic = p.options.Namespace + "%" + msg.Topic
	}

	var err error
	for retryCount := 0; retryCount < retryTime; retryCount++ {
		mq := p.selectMessageQueue(msg)
		if mq == nil {
			err = fmt.Errorf("the topic=%s route info not found", msg.Topic)
			continue
		}

		addr := internal.FindBrokerAddrByName(mq.BrokerName)
		if addr == "" {
			return fmt.Errorf("topic=%s route info not found", mq.Topic)
		}

		_err := p.client.InvokeOneWay(addr, p.buildSendRequest(mq, msg), 3*time.Second)
		if _err != nil {
			err = _err
			continue
		}
		return nil
	}
	return err
}

func (p *defaultProducer) buildSendRequest(mq *primitive.MessageQueue,
	msg *primitive.Message) *remote.RemotingCommand {
	if msg.Properties == nil {
		msg.Properties = make(map[string]string, 0)
	}
	if !msg.Batch && msg.Properties[primitive.PropertyUniqueClientMessageIdKeyIndex] == "" {
		msg.Properties[primitive.PropertyUniqueClientMessageIdKeyIndex] = primitive.CreateUniqID()
	}
	sysFlag := 0
	v, ok := msg.Properties[primitive.PropertyTransactionPrepared]
	if ok {
		tranMsg, err := strconv.ParseBool(v)
		if err == nil && tranMsg {
			sysFlag |= primitive.TransactionPreparedType
		}
	}

	req := &internal.SendMessageRequest{
		ProducerGroup:  p.group,
		Topic:          mq.Topic,
		QueueId:        mq.QueueId,
		SysFlag:        sysFlag,
		BornTimestamp:  time.Now().UnixNano() / int64(time.Millisecond),
		Flag:           msg.Flag,
		Properties:     primitive.MarshalPropeties(msg.Properties),
		ReconsumeTimes: 0,
		UnitMode:       p.options.UnitMode,
		Batch:          false,
	}

	return remote.NewRemotingCommand(internal.ReqSendMessage, req, msg.Body)
}

func (p *defaultProducer) selectMessageQueue(msg *primitive.Message) *primitive.MessageQueue {
	topic := msg.Topic

	v, exist := p.publishInfo.Load(topic)
	if !exist {
		p.client.UpdatePublishInfo(topic, internal.UpdateTopicRouteInfo(topic))
		v, exist = p.publishInfo.Load(topic)
	}

	if !exist {
		return nil
	}

	result := v.(*internal.TopicPublishInfo)
	if result == nil || !result.HaveTopicRouterInfo {
		return nil
	}

	if result.MqList != nil && len(result.MqList) <= 0 {
		rlog.Error("can not find proper message queue")
		return nil
	}

	i := p.options.Selector.Select(msg, len(result.MqList))
	return result.MqList[i]
}

func (p *defaultProducer) PublishTopicList() []string {
	topics := make([]string, 0)
	p.publishInfo.Range(func(key, value interface{}) bool {
		topics = append(topics, key.(string))
		return true
	})
	return topics
}

func (p *defaultProducer) UpdateTopicPublishInfo(topic string, info *internal.TopicPublishInfo) {
	if topic == "" || info == nil {
		return
	}
	p.publishInfo.Store(topic, info)
}

func (p *defaultProducer) IsPublishTopicNeedUpdate(topic string) bool {
	v, exist := p.publishInfo.Load(topic)
	if !exist {
		return true
	}
	info := v.(*internal.TopicPublishInfo)
	return info.MqList == nil || len(info.MqList) == 0
}

func (p *defaultProducer) IsUnitMode() bool {
	return false
}

type transactionProducer struct {
	producer *defaultProducer
	listener primitive.TransactionListener
}

// TODO: checkLocalTransaction
func NewTransactionProducer(listener primitive.TransactionListener, opts ...Option) (*transactionProducer, error) {
	producer, err := NewDefaultProducer(opts...)
	if err != nil {
		return nil, errors.Wrap(err, "NewDefaultProducer failed.")
	}
	return &transactionProducer{
		producer: producer,
		listener: listener,
	}, nil
}

func (tp *transactionProducer) Start() error {
	go tp.checkTransactionState()
	return tp.producer.Start()
}
func (tp *transactionProducer) Shutdown() error {
	return tp.producer.Shutdown()
}

// TODO: check addr
func (tp *transactionProducer) checkTransactionState() {
	for ch := range tp.producer.callbackCh {
		switch callback := ch.(type) {
		case internal.CheckTransactionStateCallback:
			localTransactionState := tp.listener.CheckLocalTransaction(callback.Msg)
			uniqueKey, existed := callback.Msg.Properties[primitive.PropertyUniqueClientMessageIdKeyIndex]
			if !existed {
				uniqueKey = callback.Msg.MsgId
			}
			header := &internal.EndTransactionRequestHeader{
				CommitLogOffset:      callback.Header.CommitLogOffset,
				ProducerGroup:        tp.producer.group,
				TranStateTableOffset: callback.Header.TranStateTableOffset,
				FromTransactionCheck: true,
				MsgID:                uniqueKey,
				TransactionId:        callback.Header.TransactionId,
				CommitOrRollback:     tp.transactionState(localTransactionState),
			}

			req := remote.NewRemotingCommand(internal.ReqENDTransaction, header, nil)
			req.Remark = tp.errRemark(nil)

			tp.producer.client.InvokeOneWay(callback.Addr.String(), req, tp.producer.options.SendMsgTimeout)
		default:
			rlog.Error("unknow type %v", ch)
		}
	}
}

func (tp *transactionProducer) SendMessageInTransaction(ctx context.Context, msg *primitive.Message) (*primitive.TransactionSendResult, error) {
	if msg.Properties == nil {
		msg.Properties = make(map[string]string, 0)
	}
	msg.Properties[primitive.PropertyTransactionPrepared] = "true"
	msg.Properties[primitive.PropertyProducerGroup] = tp.producer.options.GroupName

	rsp, err := tp.producer.SendSync(ctx, msg)
	if err != nil {
		return nil, err
	}
	localTransactionState := primitive.UnknowState
	switch rsp.Status {
	case primitive.SendOK:
		if len(rsp.TransactionID) > 0 {
			msg.Properties["__transactionId__"] = rsp.TransactionID
		}
		transactionId := msg.Properties[primitive.PropertyUniqueClientMessageIdKeyIndex]
		if len(transactionId) > 0 {
			msg.TransactionId = transactionId
		}
		localTransactionState = tp.listener.ExecuteLocalTransaction(*msg)
		if localTransactionState != primitive.CommitMessageState {
			rlog.Errorf("executeLocalTransactionBranch return %v with msg: %v\n", localTransactionState, msg)
		}

	case primitive.SendFlushDiskTimeout, primitive.SendFlushSlaveTimeout, primitive.SendSlaveNotAvailable:
		localTransactionState = primitive.RollbackMessageState
	default:
	}

	tp.endTransaction(*rsp, err, localTransactionState)

	transactionSendResult := &primitive.TransactionSendResult{
		SendResult: rsp,
		State:      localTransactionState,
	}

	return transactionSendResult, nil
}

func (tp *transactionProducer) endTransaction(result primitive.SendResult, err error, state primitive.LocalTransactionState) error {
	var msgID *primitive.MessageID
	if len(result.OffsetMsgID) > 0 {
		msgID, _ = primitive.UnmarshalMsgID([]byte(result.OffsetMsgID))
	} else {
		msgID, _ = primitive.UnmarshalMsgID([]byte(result.MsgID))
	}
	// 估计没有反序列化回来
	brokerAddr := internal.FindBrokerAddrByName(result.MessageQueue.BrokerName)
	requestHeader := &internal.EndTransactionRequestHeader{
		TransactionId:        result.TransactionID,
		CommitLogOffset:      msgID.Offset,
		ProducerGroup:        tp.producer.group,
		TranStateTableOffset: result.QueueOffset,
		MsgID:                result.MsgID,
		CommitOrRollback:     tp.transactionState(state),
	}

	req := remote.NewRemotingCommand(internal.ReqENDTransaction, requestHeader, nil)
	req.Remark = tp.errRemark(err)

	return tp.producer.client.InvokeOneWay(brokerAddr, req, tp.producer.options.SendMsgTimeout)
}

func (tp *transactionProducer) errRemark(err error) string {
	if err != nil {
		return "executeLocalTransactionBranch exception: " + err.Error()
	}
	return ""
}

func (tp *transactionProducer) transactionState(state primitive.LocalTransactionState) int {
	switch state {
	case primitive.CommitMessageState:
		return primitive.TransactionCommitType
	case primitive.RollbackMessageState:
		return primitive.TransactionRollbackType
	case primitive.UnknowState:
		return primitive.TransactionNotType
	default:
		return primitive.TransactionNotType
	}
}
