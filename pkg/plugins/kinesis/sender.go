// Copyright 2020 Comcast Cable Communications Management, LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kinesis

import (
	"context"
	"encoding/json"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/goccy/go-yaml"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/xmidt-org/ears/pkg/event"
	pkgplugin "github.com/xmidt-org/ears/pkg/plugin"
	"github.com/xmidt-org/ears/pkg/secret"
	"github.com/xmidt-org/ears/pkg/sender"
	"github.com/xmidt-org/ears/pkg/tenant"
	"time"
)

func NewSender(tid tenant.Id, plugin string, name string, config interface{}, secrets secret.Vault) (sender.Sender, error) {
	var cfg SenderConfig
	var err error
	switch c := config.(type) {
	case string:
		err = yaml.Unmarshal([]byte(c), &cfg)
	case []byte:
		err = yaml.Unmarshal(c, &cfg)
	case SenderConfig:
		cfg = c
	case *SenderConfig:
		cfg = *c
	}
	if err != nil {
		return nil, &pkgplugin.InvalidConfigError{
			Err: err,
		}
	}
	cfg = cfg.WithDefaults()
	err = cfg.Validate()
	if err != nil {
		return nil, err
	}
	s := &Sender{
		name:   name,
		plugin: plugin,
		tid:    tid,
		config: cfg,
		logger: event.GetEventLogger(),
	}
	s.initPlugin()
	return s, nil
}

func (s *Sender) initPlugin() error {
	s.Lock()
	defer s.Unlock()
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(endpoints.UsWest2RegionID),
	})
	if nil != err {
		return err
	}
	_, err = sess.Config.Credentials.Get()
	if nil != err {
		return err
	}
	s.kinesisService = kinesis.New(sess)
	_, err = s.kinesisService.DescribeStream(&kinesis.DescribeStreamInput{StreamName: aws.String(s.config.StreamName)})
	if err != nil {
		return err
	}
	s.done = make(chan struct{})
	s.startTimedSender()
	return nil
}

func (s *Sender) startTimedSender() {
	go func() {
		for {
			select {
			case <-s.done:
				s.logger.Info().Str("op", "Kinesis.timedSender").Str("name", s.Name()).Str("tid", s.Tenant().ToString()).Int("sendCount", s.count).Msg("stopping kinesis sender")
				return
			case <-time.After(time.Duration(*s.config.SendTimeout) * time.Second):
			}
			s.Lock()
			if s.eventBatch == nil {
				s.eventBatch = make([]event.Event, 0)
			}
			evtBatch := s.eventBatch
			s.eventBatch = make([]event.Event, 0)
			s.Unlock()
			if len(evtBatch) > 0 {
				s.send(evtBatch)
			}
		}
	}()
}

func (s *Sender) Count() int {
	s.Lock()
	defer s.Unlock()
	return s.count
}

func (s *Sender) StopSending(ctx context.Context) {
	s.Lock()
	if s.done != nil {
		s.done <- struct{}{}
		s.done = nil
	}
	s.Unlock()
}

func (s *Sender) send(events []event.Event) {
	if len(events) == 0 {
		return
	}
	batchReqs := []*kinesis.PutRecordsRequestEntry{}
	for idx, evt := range events {
		if idx == 0 {
			log.Ctx(evt.Context()).Debug().Str("op", "Kinesis.sendWorker").Str("name", s.Name()).Str("tid", s.Tenant().ToString()).Int("eventIdx", idx).Int("batchSize", len(events)).Int("sendCount", s.count).Msg("send message batch")
		}
		buf, err := json.Marshal(evt.Payload())
		if err != nil {
			continue
		}
		putReq := kinesis.PutRecordsRequestEntry{
			Data:         buf,
			PartitionKey: aws.String(uuid.New().String()),
		}
		batchReqs = append(batchReqs, &putReq)
	}
	batchPut := kinesis.PutRecordsInput{
		Records:    batchReqs,
		StreamName: aws.String(s.config.StreamName),
	}
	putResults, err := s.kinesisService.PutRecordsWithContext(events[0].Context(), &batchPut)
	successCount := 0
	if err != nil {
		log.Ctx(events[0].Context()).Error().Str("op", "Kinesis.sendWorker").Str("name", s.Name()).Str("tid", s.Tenant().ToString()).Int("batchSize", len(events)).Msg("batch send error: " + err.Error())
		for idx := range events {
			events[idx].Nack(err)
		}
	} else {
		for idx, putResult := range putResults.Records {
			if putResult.ErrorCode == nil {
				successCount++
				events[idx].Ack()
			} else {
				events[idx].Nack(err)
			}
		}
	}
	s.Lock()
	s.count += successCount
	s.Unlock()
}

func (s *Sender) Send(e event.Event) {
	s.Lock()
	if s.eventBatch == nil {
		s.eventBatch = make([]event.Event, 0)
	}
	s.eventBatch = append(s.eventBatch, e)
	if len(s.eventBatch) >= *s.config.MaxNumberOfMessages {
		eventBatch := s.eventBatch
		s.eventBatch = make([]event.Event, 0)
		s.Unlock()
		s.send(eventBatch)
	} else {
		s.Unlock()
	}
}

func (s *Sender) Unwrap() sender.Sender {
	return s
}

func (s *Sender) Config() interface{} {
	return s.config
}

func (s *Sender) Name() string {
	return s.name
}

func (s *Sender) Plugin() string {
	return s.plugin
}

func (s *Sender) Tenant() tenant.Id {
	return s.tid
}